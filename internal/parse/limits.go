package parse

import (
	"errors"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// ErrFeedTooComplex is returned when a document exceeds the structural
// limits below. The response body cap (config.DefaultMaxBodyBytes) bounds
// the input but not the memory used to parse it: gofeed builds maps for
// every unknown element and recurses into nested ones, the XML pull parser
// under it copies the namespace bindings in scope for every element, and
// JSON Feed decoding allocates a struct per item. A hostile or absurd
// 10 MB document would otherwise need gigabytes.
var ErrFeedTooComplex = errors.New("feed document is too deeply nested or has too many elements")

// Defensive limits on untrusted feed documents. Unlike the 10 MB body cap in
// internal/config they are not tunable; each is far above what real feeds
// use, and low enough that the worst case parses in tens of megabytes.
const (
	// maxXMLDepth bounds element nesting in an RSS or Atom document; gofeed
	// parses unknown elements recursively. Real feeds, XHTML content
	// included, stay below 30.
	maxXMLDepth = 100
	// maxXMLNodes bounds the elements plus attributes of an RSS or Atom
	// document. A 10 MB feed of escaped HTML has a few tens of thousands;
	// XHTML content can bring a large one to about 150,000.
	maxXMLNodes = 200_000
	// maxXMLNamespaces bounds the namespace declarations in scope at any
	// element: the pull parser copies them all for every element.
	maxXMLNamespaces = 64
	// maxJSONValues bounds the values (objects, arrays and scalars) of a
	// JSON Feed.
	maxJSONValues = 200_000
	// maxFeedItems bounds the items or entries of one document, whatever
	// its format (a feed of thousands of items must still parse).
	maxFeedItems = 10_000

	// maxFieldBytes bounds the raw bytes of one summary or content field
	// that are rendered; the rest is cut at a UTF-8 boundary. The UI only
	// uses the first image and at most config.SummaryFullMaxChars of
	// visible text, which takes far fewer bytes even with markup.
	maxFieldBytes = 64 << 10
	// maxHTMLTreeNodes bounds the nodes the HTML tree builder may create for
	// one field (an upper bound computed before parsing; see
	// treeFragment). Markup past the point where the bound is reached is
	// not rendered.
	maxHTMLTreeNodes = 100_000
	// renderSlackBytes is the growth allowed when a field is rendered, on
	// top of twice its length: resolved URLs and the rel/target attributes
	// added to links make short snippets grow. Output beyond that is
	// replaced by the field's text (see fallbackHTML).
	renderSlackBytes = 4 << 10
	// fallbackTextRunes is how much visible text a field keeps when its
	// HTML cannot be rendered within the limits: all the UI can show.
	fallbackTextRunes = config.SummaryFullMaxChars

	// maxTitleRunes bounds feed and entry titles and author names. It
	// equals the web layer's limit on user-entered names, so a feed title
	// can be stored as a subscription title as is.
	maxTitleRunes = 200
	// maxURLBytes bounds the URLs this package returns (site, icon, entry
	// link, thumbnail); longer ones are dropped.
	maxURLBytes = 2048
)

// renderLimit is the largest rendered output accepted for an input of n
// bytes.
func renderLimit(n int) int { return 2*n + renderSlackBytes }
