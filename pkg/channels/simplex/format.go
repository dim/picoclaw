package simplex

import (
	"strconv"
	"strings"

	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

// markdownToSimplex converts the GitHub-flavored Markdown the agent emits into
// SimpleX Chat's native lightweight formatting. SimpleX clients render neither
// HTML nor standard Markdown; they only understand a few inline markers
// (verified against Simplex/Chat/Markdown.hs) and auto-link bare URLs:
//
//	*bold*  _italic_  ~strikethrough~  `code`
//
// Note the asterisk is INVERTED versus GitHub: SimpleX `*x*` is bold and `_x_`
// is italic. Constructs SimpleX cannot render are flattened to readable text:
// headings -> *bold* line, links -> "label (url)", bullet/ordered lists ->
// "• item" / "1. item" lines, tables -> pipe-separated rows, fenced code ->
// wrapped in `…`. It reuses the gomarkdown dependency already used by the
// Matrix channel; plain text without Markdown passes through unchanged.
func markdownToSimplex(src string) string {
	if strings.TrimSpace(src) == "" {
		return src
	}
	doc := parser.NewWithExtensions(parser.CommonExtensions).Parse([]byte(src))
	return strings.TrimSpace(renderBlocks(doc.GetChildren(), false))
}

// renderBlocks renders block-level nodes, separating top-level blocks with a
// blank line and tight contexts (list items) with a single newline.
func renderBlocks(nodes []ast.Node, tight bool) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if s := renderBlock(n); s != "" {
			parts = append(parts, s)
		}
	}
	sep := "\n\n"
	if tight {
		sep = "\n"
	}
	return strings.Join(parts, sep)
}

func renderBlock(n ast.Node) string {
	switch t := n.(type) {
	case *ast.Heading:
		return "*" + strings.TrimSpace(renderInlineChildren(n)) + "*"
	case *ast.List:
		return renderList(t)
	case *ast.CodeBlock:
		return renderCodeBlock(t)
	case *ast.Table:
		return renderTable(t)
	case *ast.Paragraph:
		return renderInlineChildren(n)
	default:
		// Paragraph, blockquote and anything else: render inner content.
		return renderInlineChildren(n)
	}
}

func renderList(list *ast.List) string {
	ordered := list.ListFlags&ast.ListTypeOrdered != 0
	num := list.Start
	if num == 0 {
		num = 1
	}
	lines := make([]string, 0, len(list.GetChildren()))
	for _, item := range list.GetChildren() {
		marker := "• "
		if ordered {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		content := renderBlocks(item.GetChildren(), true)
		lines = append(lines, indentUnderMarker(content, marker))
	}
	return strings.Join(lines, "\n")
}

// indentUnderMarker prefixes the first line with marker and aligns the rest
// (e.g. nested lists) underneath it.
func indentUnderMarker(content, marker string) string {
	lines := strings.Split(content, "\n")
	pad := strings.Repeat(" ", len([]rune(marker)))
	for i := range lines {
		if i == 0 {
			lines[i] = marker + lines[i]
		} else {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func renderCodeBlock(cb *ast.CodeBlock) string {
	code := strings.TrimRight(string(cb.Literal), "\n")
	// SimpleX snippets use a single backtick and may span lines. If the code
	// contains a backtick we cannot wrap it safely, so leave it as plain text.
	if code == "" || strings.Contains(code, "`") {
		return code
	}
	return "`" + code + "`"
}

func renderTable(tbl *ast.Table) string {
	var rows [][]string
	hasHeader := false
	for _, section := range tbl.GetChildren() {
		if _, ok := section.(*ast.TableHeader); ok {
			hasHeader = true
		}
		for _, row := range section.GetChildren() {
			if _, ok := row.(*ast.TableRow); !ok {
				continue
			}
			var cells []string
			for _, cell := range row.GetChildren() {
				cells = append(cells, strings.ReplaceAll(strings.TrimSpace(renderInlineChildren(cell)), "\n", " "))
			}
			rows = append(rows, cells)
		}
	}
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows)+1)
	for i, cells := range rows {
		lines = append(lines, strings.Join(cells, " | "))
		if i == 0 && hasHeader {
			dashes := make([]string, len(cells))
			for j := range dashes {
				dashes[j] = "---"
			}
			lines = append(lines, strings.Join(dashes, " | "))
		}
	}
	return strings.Join(lines, "\n")
}

func renderInlineChildren(n ast.Node) string {
	var b strings.Builder
	for _, child := range n.GetChildren() {
		b.WriteString(renderInline(child))
	}
	return b.String()
}

func renderInline(n ast.Node) string {
	switch t := n.(type) {
	case *ast.Text:
		return string(t.Literal)
	case *ast.Code:
		return "`" + string(t.Literal) + "`"
	case *ast.Strong:
		return "*" + renderInlineChildren(n) + "*"
	case *ast.Emph:
		return "_" + renderInlineChildren(n) + "_"
	case *ast.Del:
		return "~" + renderInlineChildren(n) + "~"
	case *ast.Link:
		return renderLinkLike(renderInlineChildren(n), string(t.Destination))
	case *ast.Image:
		return renderLinkLike(renderInlineChildren(n), string(t.Destination))
	case *ast.Softbreak, *ast.Hardbreak:
		return "\n"
	default:
		if leaf := n.AsLeaf(); leaf != nil {
			return string(leaf.Literal)
		}
		return renderInlineChildren(n)
	}
}

// renderLinkLike formats a link/image as "label (url)", or just the url when
// the label is empty or identical (SimpleX auto-links bare URLs).
func renderLinkLike(label, dest string) string {
	label = strings.TrimSpace(label)
	if dest == "" || label == dest {
		return label
	}
	if label == "" {
		return dest
	}
	return label + " (" + dest + ")"
}
