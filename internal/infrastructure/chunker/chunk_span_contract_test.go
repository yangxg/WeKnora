package chunker

import (
	"strings"
	"testing"
)

// The span contract every downstream citation consumer depends on.
//
// Chunk documents the invariant informally ("Content holds exactly the text
// between Start and End"), and mergeUnits deliberately relaxes it: a table
// header is prepended to Content as a zero-width unit so a continuation row
// keeps its column names, while Start/End stay on the row itself. The weaker
// contract that survives is the one callers can actually rely on:
//
//	0 <= Start < End <= runeLen(text)   and   Content ends with text[Start:End]
//
// A consumer that re-reads a chunk at its own range in the source document —
// citation verification, quote anchoring, highlighting — is correct exactly
// when this holds. TestSplitText_TableChunking_PositionInvariant already pins
// it for one table under the legacy path; this file holds every tier to it
// across the shapes a real document mixes, so a chunker change cannot silently
// move offsets away from the content they address.
func TestChunkSpanContract_ContentEndsAtItsOwnRange(t *testing.T) {
	strategies := []string{
		StrategyLegacy,
		StrategyRecursive,
		StrategyHeading,
		StrategyHeuristic,
		StrategyAuto,
	}

	for _, corpus := range spanContractCorpora() {
		for _, strategy := range strategies {
			t.Run(corpus.name+"/"+strategy, func(t *testing.T) {
				cfg := corpus.cfg
				cfg.Strategy = strategy
				chunks := Split(corpus.text, cfg)
				if len(chunks) == 0 {
					t.Fatal("expected at least one chunk")
				}
				assertSpanContract(t, corpus.text, chunks)
			})
		}
	}
}

// The prepended header has to actually occur, or the contract above is being
// proved on documents that never exercise the case it exists for.
func TestChunkSpanContract_TablesDoPrependAHeaderOutsideTheRange(t *testing.T) {
	for _, name := range []string{"single_table", "two_tables", "headings_and_table"} {
		t.Run(name, func(t *testing.T) {
			corpus := spanContractCorpus(t, name)
			chunks := Split(corpus.text, corpus.cfg)
			if prefixed := countPrefixed(corpus.text, chunks); prefixed == 0 {
				t.Errorf("no chunk of %d carries text outside its own range: the "+
					"table-header prepend is no longer exercised here", len(chunks))
			}
		})
	}
}

// Parent-child splitting re-splits each parent's own Content and shifts the
// child offsets by that parent's Start. The arithmetic holds only while a
// parent's Content *is* text[Start:End] — and the prepended table header is
// exactly the case where it is not, which pushes every child of that parent
// past the text it addresses.
//
// The law asserted here is that one correspondence: parents always satisfy the
// contract, and children satisfy it iff no parent carries a prepended header.
// A consumer that re-reads chunks at their ranges can therefore rely on
// parent-child output only for documents without tables. If a fix lands
// upstream this test fails on the table corpora, which is the signal to widen
// that restriction rather than a regression.
func TestChunkSpanContract_ParentChildKeepsRangesOnlyWithoutPrependedHeaders(t *testing.T) {
	for _, corpus := range spanContractCorpora() {
		t.Run(corpus.name, func(t *testing.T) {
			parentCfg := corpus.cfg
			parentCfg.ChunkSize = 160
			childCfg := corpus.cfg
			childCfg.ChunkSize = 60

			result := SplitParentChild(corpus.text, parentCfg, childCfg)
			if len(result.Parents) == 0 || len(result.Children) == 0 {
				t.Fatal("expected both levels to produce chunks")
			}

			// Parents are cut from the document itself, so they hold either way.
			assertSpanContract(t, corpus.text, result.Parents)

			children := make([]Chunk, 0, len(result.Children))
			for _, child := range result.Children {
				children = append(children, child.Chunk)
			}
			prefixedParents := countPrefixed(corpus.text, result.Parents)
			offRange := countOffContract(corpus.text, children)

			if prefixedParents == 0 {
				if offRange != 0 {
					t.Errorf("%d of %d children lost their range although no parent "+
						"carries text outside its own", offRange, len(children))
					assertSpanContract(t, corpus.text, children)
				}
				return
			}
			if offRange == 0 {
				t.Errorf("every child kept its range although %d of %d parents carry a "+
					"prepended header: the shift is fixed, and consumers pinned to "+
					"single-level chunking can be widened",
					prefixedParents, len(result.Parents))
			}
			t.Logf("known limitation: %d of %d children are unreadable at their range "+
				"because %d parents carry a prepended header",
				offRange, len(children), prefixedParents)
		})
	}
}

type spanContractCase struct {
	name string
	text string
	cfg  SplitterConfig
}

// The document shapes a real corpus mixes: Chinese prose, one table long enough
// to force header repetition, two adjacent tables, spans the splitter protects
// from being cut, prose split with a wide overlap, and headings around a table.
func spanContractCorpora() []spanContractCase {
	small := SplitterConfig{
		ChunkSize:    80,
		ChunkOverlap: 20,
		Separators:   []string{"\n\n", "\n", "。", " "},
	}
	return []spanContractCase{
		{name: "chinese_prose", cfg: small, text: spanContractProse},
		{name: "single_table", cfg: small, text: spanContractTable},
		{name: "two_tables", cfg: small, text: spanContractTwoTables},
		{
			name: "protected_spans",
			cfg:  SplitterConfig{ChunkSize: 120, ChunkOverlap: 20, Separators: []string{"\n\n", "\n"}},
			text: spanContractFormulaCode,
		},
		{
			name: "wide_overlap",
			cfg:  SplitterConfig{ChunkSize: 60, ChunkOverlap: 40, Separators: []string{"\n\n", "\n", "。"}},
			text: spanContractProse,
		},
		{
			name: "headings_and_table",
			cfg:  SplitterConfig{ChunkSize: 100, ChunkOverlap: 20, Separators: []string{"\n\n", "\n", "。"}},
			text: spanContractHeadings,
		},
	}
}

func spanContractCorpus(t *testing.T, name string) spanContractCase {
	t.Helper()
	for _, corpus := range spanContractCorpora() {
		if corpus.name == name {
			return corpus
		}
	}
	t.Fatalf("unknown corpus %q", name)
	return spanContractCase{}
}

// assertSpanContract reports every chunk whose range cannot be read back in the
// source document, or whose Content does not end at that range.
func assertSpanContract(t *testing.T, text string, chunks []Chunk) {
	t.Helper()
	runes := []rune(text)
	for i, c := range chunks {
		if c.Start < 0 || c.End > len(runes) {
			t.Errorf("chunk[%d]: range [%d,%d) outside the document of %d runes",
				i, c.Start, c.End, len(runes))
			continue
		}
		if c.End <= c.Start {
			t.Errorf("chunk[%d]: empty range [%d,%d) — a chunk addressing nothing "+
				"would match any content at all", i, c.Start, c.End)
			continue
		}
		if addressed := string(runes[c.Start:c.End]); !strings.HasSuffix(c.Content, addressed) {
			t.Errorf("chunk[%d]: Content does not end at text[%d:%d]"+
				"\n  text slice: %q"+
				"\n  content:    %q",
				i, c.Start, c.End, addressed, c.Content)
		}
	}
}

// countOffContract counts the chunks that break the contract.
func countOffContract(text string, chunks []Chunk) int {
	runes := []rune(text)
	off := 0
	for _, c := range chunks {
		if c.Start < 0 || c.End > len(runes) || c.End <= c.Start ||
			!strings.HasSuffix(c.Content, string(runes[c.Start:c.End])) {
			off++
		}
	}
	return off
}

// countPrefixed counts the contract-abiding chunks that carry text ahead of the
// range they address — the prepended table header, in practice.
func countPrefixed(text string, chunks []Chunk) int {
	runes := []rune(text)
	prefixed := 0
	for _, c := range chunks {
		if c.Start < 0 || c.End > len(runes) || c.End <= c.Start {
			continue
		}
		if strings.HasSuffix(c.Content, string(runes[c.Start:c.End])) &&
			len([]rune(c.Content)) > c.End-c.Start {
			prefixed++
		}
	}
	return prefixed
}

const spanContractProse = "医保支付方式改革试点自二零二五年起在全国范围内推进。" +
	"试点覆盖三十个统筹地区，按病种分值付费与按床日付费并行。\n" +
	"\n" +
	"评估结果显示，住院次均费用下降百分之七点二，平均住院日缩短零点八天。" +
	"各地在配套的信息系统建设上进度不一，部分地区仍以手工上报为主。\n" +
	"\n" +
	"下一阶段将扩大到全部统筹地区，并把门诊慢特病纳入同一结算口径。\n"

const spanContractTable = "# 分级覆盖情况\n" +
	"\n" +
	"| 层级 | 目录内项目 | 已覆盖 |\n" +
	"| --- | --- | --- |\n" +
	"| 一级 | 180 | 142 |\n" +
	"| 二级 | 240 | 121 |\n" +
	"| 三级 | 310 | 208 |\n" +
	"| 四级 | 95 | 61 |\n" +
	"| 五级 | 140 | 87 |\n" +
	"\n" +
	"层级划分沿用二零二五年目录。\n"

const spanContractTwoTables = spanContractTable +
	"\n" +
	"| 年度 | 结算笔数 | 拒付率 |\n" +
	"| --- | --- | --- |\n" +
	"| 2024 | 12800 | 0.031 |\n" +
	"| 2025 | 15400 | 0.024 |\n" +
	"| 2026 | 17100 | 0.019 |\n" +
	"\n" +
	"拒付率按年度结算笔数加权。\n"

const spanContractFormulaCode = "## 计算口径\n" +
	"\n" +
	"次均费用按下式计算：$C = \\frac{\\sum_{i=1}^{n} c_i}{n}$，其中 $c_i$ 为第 i 例的结算金额。\n" +
	"\n" +
	"```python\n" +
	"def average_cost(settlements):\n" +
	"    return sum(settlements) / len(settlements)\n" +
	"```\n" +
	"\n" +
	"该口径与统计年鉴一致，未做去极值处理。\n"

const spanContractHeadings = "# 二零二六年支付方式改革评估\n" +
	"\n" +
	"## 一、覆盖面\n" +
	"\n" +
	"改革已覆盖三十个统筹地区，其中二十四个完成了信息系统对接。\n" +
	"\n" +
	"## 二、费用变化\n" +
	"\n" +
	"| 地区 | 次均费用 | 同比 |\n" +
	"| --- | --- | --- |\n" +
	"| 北京 | 12580 | -0.072 |\n" +
	"| 上海 | 13140 | -0.058 |\n" +
	"| 广州 | 10920 | -0.081 |\n" +
	"\n" +
	"## 三、后续安排\n" +
	"\n" +
	"下一阶段把门诊慢特病纳入同一结算口径。\n"
