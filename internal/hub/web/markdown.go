package web

import (
	"bytes"
	"html"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// md renders Transcript text: GFM, code highlighted with chroma CSS classes,
// and raw HTML shown as text (hub.md §4.6).
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(highlighting.WithFormatOptions(chromahtml.WithClasses(true))),
	),
	goldmark.WithRendererOptions(
		renderer.WithNodeRenderers(util.Prioritized(escapeHTML{}, 0)),
	),
)

func renderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "<pre>" + html.EscapeString(src) + "</pre>"
	}
	return buf.String()
}

// escapeHTML renders raw HTML nodes as escaped text instead of omitting or
// passing them through.
type escapeHTML struct{}

func (escapeHTML) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(ast.KindRawHTML, renderRawHTML)
	r.Register(ast.KindHTMLBlock, renderHTMLBlock)
}

func renderRawHTML(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	segs := n.(*ast.RawHTML).Segments
	for i := 0; i < segs.Len(); i++ {
		s := segs.At(i)
		w.WriteString(html.EscapeString(string(s.Value(src))))
	}
	return ast.WalkSkipChildren, nil
}

func renderHTMLBlock(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	b := n.(*ast.HTMLBlock)
	w.WriteString(`<p class="raw-html">`)
	lines := b.Lines()
	for i := 0; i < lines.Len(); i++ {
		s := lines.At(i)
		w.WriteString(html.EscapeString(string(s.Value(src))))
	}
	if b.HasClosure() {
		w.WriteString(html.EscapeString(string(b.ClosureLine.Value(src))))
	}
	w.WriteString("</p>\n")
	return ast.WalkContinue, nil
}

// chromaCSS is the highlighting stylesheet: a light style, and a dark one
// selected by prefers-color-scheme.
var chromaCSS = func() []byte {
	var buf bytes.Buffer
	f := chromahtml.New(chromahtml.WithClasses(true))
	f.WriteCSS(&buf, styles.Get("github"))
	buf.WriteString("@media (prefers-color-scheme: dark) {\n")
	f.WriteCSS(&buf, styles.Get("github-dark"))
	buf.WriteString("}\n")
	return buf.Bytes()
}()
