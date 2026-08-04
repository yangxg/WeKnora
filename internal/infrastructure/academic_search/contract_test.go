package academic_search

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bodyShapedNames marks a field as carrying — or being able to carry — the text
// of a work rather than its identity.
//
// The academic lane produces no governed body (ResearchFlow ADR-0012 §2), and
// the way that rule is kept is structural: no type on this path has anywhere to
// put one. A registry's abstract is not the work, and a reader downstream of a
// governed revision has no field in which to learn that only an abstract was
// governed, so the refusal has to happen where the text would first be held.
var bodyShapedNames = []string{
	"abstract", "summary", "snippet", "content", "body",
	"fulltext", "text", "markdown", "html", "pdf",
}

func assertFieldSet(t *testing.T, typ reflect.Type, want map[string]reflect.Kind) {
	t.Helper()
	got := make(map[string]reflect.Kind, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		got[field.Name] = field.Type.Kind()
		lowered := strings.ToLower(field.Name)
		for _, banned := range bodyShapedNames {
			if strings.Contains(lowered, banned) {
				t.Errorf("%s.%s is body-shaped; the academic lane carries identity only (ADR-0012 §2)",
					typ.Name(), field.Name)
			}
		}
	}
	for name, kind := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("%s is missing field %s", typ.Name(), name)
			continue
		}
		if actual != kind {
			t.Errorf("%s.%s is %s, want %s", typ.Name(), name, actual, kind)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("%s has unexpected field %s; widening this struct needs an ADR, not an edit",
			typ.Name(), name)
	}
}

func TestWorkCarriesIdentityOnly(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(Work{}), map[string]reflect.Kind{
		"DOI":     reflect.String,
		"ArXivID": reflect.String,
		"PMID":    reflect.String,
		"Title":   reflect.String,
		"Year":    reflect.Int,
		"Authors": reflect.Slice,
		"Venue":   reflect.String,
		"Type":    reflect.String,
	})
}

func TestResponseCarriesNoBodyEither(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(Response{}), map[string]reflect.Kind{
		"Works":   reflect.Slice,
		"Total":   reflect.Int,
		"Dropped": reflect.Int,
	})
}

func TestOptionsCannotAskForContent(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(Options{}), map[string]reflect.Kind{
		"Count":      reflect.Int,
		"From":       reflect.Struct,
		"To":         reflect.Struct,
		"WorkTypes":  reflect.Slice,
		"OpenAccess": reflect.String,
	})
}

func TestAPIErrorCarriesNoVendorMessage(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(APIError{}), map[string]reflect.Kind{
		"Source":     reflect.String,
		"HTTPStatus": reflect.Int,
		"Code":       reflect.String,
		"RequestID":  reflect.String,
		"RetryAfter": reflect.Int64,
		"Retryable":  reflect.Bool,
		"Err":        reflect.Interface,
	})
}

func TestAPIErrorMessageOmitsEmptyParts(t *testing.T) {
	err := &APIError{Source: "crossref", HTTPStatus: 429, Retryable: true}
	message := err.Error()
	if !strings.Contains(message, "crossref") || !strings.Contains(message, "429") {
		t.Fatalf("error message %q should name the source and status", message)
	}
	if strings.Contains(message, "code=") || strings.Contains(message, "request_id=") {
		t.Fatalf("error message %q should omit the parts it does not have", message)
	}
}

func TestAPIErrorUnwrapsTheTransportError(t *testing.T) {
	inner := errors.New("dial timeout")
	err := &APIError{Source: "arxiv", Code: "TransportError", Err: inner}
	if !errors.Is(err, inner) {
		t.Fatal("APIError should unwrap to its transport error")
	}
}

// A work nobody can identify cannot be matched against the PDF a researcher
// later acquires and promotes, which is the one thing the academic candidate
// exists to make possible (ADR-0012 §2).
func TestIdentityPrefersDOIThenArXivThenPMID(t *testing.T) {
	cases := []struct {
		name string
		work Work
		want string
		ok   bool
	}{
		{"doi wins", Work{DOI: "10.1001/jama.2024.1", ArXivID: "2401.00001", PMID: "38000000"},
			"doi:10.1001/jama.2024.1", true},
		{"arxiv without a doi", Work{ArXivID: "2401.00001", PMID: "38000000"},
			"arxiv:2401.00001", true},
		{"pmid last", Work{PMID: "38000000"}, "pmid:38000000", true},
		{"a title is not an identity", Work{Title: "A study of things", Year: 2024}, "", false},
		{"blank identifiers do not count", Work{DOI: "   "}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.work.Identity()
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Identity() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeDOIStripsPrefixesAndLowercases(t *testing.T) {
	cases := map[string]string{
		"10.1001/JAMA.2024.1":              "10.1001/jama.2024.1",
		"doi:10.1001/jama.2024.1":          "10.1001/jama.2024.1",
		"DOI: 10.1001/jama.2024.1":         "10.1001/jama.2024.1",
		"https://doi.org/10.1001/jama.1":   "10.1001/jama.1",
		"http://dx.doi.org/10.1001/jama.1": "10.1001/jama.1",
		"  10.1001/jama.2024.1  ":          "10.1001/jama.2024.1",
		// Refused rather than repaired: a mangled deduplication key is worse
		// than an absent one, and the caller drops the candidate either way.
		"not-a-doi":               "",
		"10.1001":                 "",
		"":                        "",
		"https://example.com/x":   "",
		"10.1001/jama with space": "",
	}
	for input, want := range cases {
		if got := NormalizeDOI(input); got != want {
			t.Errorf("NormalizeDOI(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOptionsValidateRejectsValuesTheManifestCannotHold(t *testing.T) {
	valid := Options{Count: 10, WorkTypes: []string{WorkTypeJournalArticle}, OpenAccess: OpenAccessAny}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid profile was refused: %v", err)
	}
	if err := (Options{Count: MaxCount + 1}).Validate(); err == nil {
		t.Error("a count above MaxCount should be refused")
	}
	if err := (Options{Count: -1}).Validate(); err == nil {
		t.Error("a negative count should be refused")
	}
	if err := (Options{WorkTypes: []string{"blog-post"}}).Validate(); err == nil {
		t.Error("a work type outside the kernel vocabulary should be refused")
	}
	if err := (Options{OpenAccess: "sometimes"}).Validate(); err == nil {
		t.Error("an unknown open access value should be refused")
	}
	from := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := (Options{From: from, To: to}).Validate(); err == nil {
		t.Error("an inverted date window should be refused")
	}
}

// The pacing constants are a deployment promise, not a tuning parameter: arXiv
// counts one request every three seconds across every machine under our control
// (ADR-0012 §5). Keeping the interval inside the client means a project cannot
// edit its way past it.
func TestPacerEnforcesTheMinimumInterval(t *testing.T) {
	var now time.Time
	var slept []time.Duration
	pacer := newPacerWithClock(2*time.Second,
		func() time.Time { return now },
		func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			now = now.Add(d)
			return nil
		},
	)

	ctx := context.Background()
	if err := pacer.Wait(ctx); err != nil {
		t.Fatalf("the first call should not wait: %v", err)
	}
	if len(slept) != 0 {
		t.Fatalf("the first call slept %v", slept)
	}

	now = now.Add(500 * time.Millisecond)
	if err := pacer.Wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if len(slept) != 1 || slept[0] != 1500*time.Millisecond {
		t.Fatalf("second call slept %v, want one 1.5s sleep", slept)
	}

	now = now.Add(10 * time.Second)
	if err := pacer.Wait(ctx); err != nil {
		t.Fatalf("third wait: %v", err)
	}
	if len(slept) != 1 {
		t.Fatalf("a call after a long gap should not sleep, slept %v", slept)
	}
}

func TestPacerStopsWaitingWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pacer := newPacerWithClock(time.Hour, time.Now,
		func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	if err := pacer.Wait(ctx); err == nil {
		t.Fatal("Wait should return the context error rather than sleeping through cancellation")
	}
	if err := pacer.Wait(ctx); err == nil {
		t.Fatal("a cancelled context should keep failing")
	}
}

func TestZeroPacerNeverWaits(t *testing.T) {
	var pacer *Pacer
	if err := pacer.Wait(context.Background()); err != nil {
		t.Fatalf("a nil pacer should be a no-op, got %v", err)
	}
}
