// Command fontcheck asserts the CSS-SIDE half of the cell contract the
// tiling-glyph overlay (cplieger/web-terminal-glyphs) publishes as cell.json:
// that the CSS this image actually serves (the bundle built from the ARG-pinned
// @cplieger/web-terminal-ui) leads the contract's stack with the overlay,
// renders at the cell the glyphs are drawn for, forecloses synthesis, and
// points four exact @font-face descriptor sets per family at assets this image
// serves. The overlay pin and the UI pin move in separate Renovate PRs, so that
// pairing is the one thing no single release can check, and a bump on either
// side that moves the cell must fail here instead of reflowing the terminal
// silently.
//
// It deliberately opens NO font file, and the guarantee chain is why. State it
// here, because a reader who cannot see it either trusts a font nobody
// inspected or files the absence as a hole:
//
//   - THE BYTES are guaranteed by the Dockerfile's sha256 pins.
//     WebTerminalGlyphs.woff2, each Monaspace face and cell.json itself are
//     pinned by digest from the SAME release, so the fonts and the document
//     describing them are fixed together at the version. Re-deriving an advance
//     out of a WOFF2 would re-prove what the digest already fixes, at the cost
//     of a brotli decoder in two app repos.
//   - THE FONT'S OWN AGREEMENT with cell.json — unitsPerEm, the hmtx advance,
//     the hhea and OS/2 vertical metrics, the drawn frame — is asserted in the
//     font repo's tests/geometry tier, against the cell.json that release
//     ships. That is the only place a font file and its contract sit side by
//     side, so it is the only place the comparison means anything.
//   - THIS gate owns what neither of those can see: the CSS a DIFFERENT
//     release built, beside the fonts a third pin fetched.
//
// So -fonts is an INVENTORY of names: it answers whether an asset a @font-face
// names is a file this image serves, and says nothing about that file's
// contents.
//
// Exit 0: the CSS-side contract holds. Exit 1: it is violated — a pin has to
// move, or the CSS has to be corrected. Exit 2: usage error — the contract or
// the CSS could not be read, so the gate itself is broken and no pin should
// move.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// usageErrMsg is the one line every exit-2 path prints last, so the
// broken-gate case is greppable in a build log and cannot be mistaken for a
// contract verdict.
const usageErrMsg = "ERROR cell-contract-gate-usage: the cell contract or the served CSS is unreadable — pass -cell <cell.json> -css <served bundle> -fonts <served fonts dir> (the gate is broken — fix the gate, do not bump a pin)"

// usageDoc is what -h prints above the flag defaults. It states the contract
// NARROWLY on purpose: the one thing worse than a gate that checks little is a
// gate claiming a check it does not run, because the reader then trusts an
// input nobody inspected. The chain the package comment sets out is restated in
// one screen here, where whoever is running the binary will read it.
const usageDoc = `fontcheck: the CSS-side half of the released glyph cell contract.

CHECKS the CSS this build serves against the released cell.json: the
--font-mono stack and its order, the .term cell (font-size, line-height and
their ratio), font-synthesis, four exact @font-face descriptor sets per family,
one asset for the overlay, no ascent/descent override, and that every asset
named is one this image serves. It refuses a bundle whose cascade it cannot
rank rather than answering from source order.

OPENS NO FONT FILE. The served bytes are guaranteed by the Dockerfile's sha256
pins (the fonts and cell.json are digest-pinned from one release); the font's
own agreement with cell.json is asserted by the font repo's geometry tests.
-fonts is an inventory of filenames, never of contents.

Exit 0 the contract holds, 1 it is violated (move a pin or correct the CSS),
2 the inputs could not be read (fix the gate, move no pin).

Flags:
`

// ratioTolerance bounds the comparison of cell.json's published ratio against
// lineHeight/fontSize. The published value is a rounded decimal (1.2142857 for
// 17/14), so an exact comparison would fail on a correct contract.
const ratioTolerance = 1e-6

// descriptorPairs is the exact set of @font-face descriptor sets each family
// must declare. The overlay publishes ONE upright face, so a lone rule would
// leave the engine free to synthesise an emboldened stand-in for a bold run;
// four exact sets against the same asset foreclose that. The companion ships
// four real faces and needs the same four sets to be reachable at all.
// termSelector is the class the terminal's cell is declared on, and
// contractProperties are the declarations on it this gate compares. The
// properties are named in one place because the cascade check has to refuse an
// unresolvable declaration of ANY of them, not only of whichever one's clause
// happens to run first.
const termSelector = ".term"

var contractProperties = [4]string{"font-family", "font-synthesis", "font-size", "line-height"}

// fontMonoProperty is the custom property carrying the contract's family stack.
const fontMonoProperty = "--font-mono"

var descriptorPairs = [4]faceKey{
	{weight: "400", style: "normal"},
	{weight: "700", style: "normal"},
	{weight: "400", style: "italic"},
	{weight: "700", style: "italic"},
}

// cellContract is the subset of the released cell.json this gate reads. The
// remaining fields (the companion's unitsPerEm, advance and vertical metrics,
// the drawn frame, the generated codepoint ranges) describe the FONT FILE: they
// travel under the same release digest as the font, and the font repo's
// tests/geometry tier asserts them against that font. What a consumer owes is
// the CSS-side cell those numbers were chosen for.
type cellContract struct {
	Companion struct {
		Family string `json:"family"`
	} `json:"companion"`
	Family string   `json:"family"`
	Stack  []string `json:"stack"`
	Cell   struct {
		FontSize   float64 `json:"fontSize"`
		LineHeight float64 `json:"lineHeight"`
		Ratio      float64 `json:"ratio"`
	} `json:"cell"`
}

// faceKey identifies one @font-face descriptor set.
type faceKey struct {
	weight string
	style  string
}

// fontFace is one @font-face rule reduced to the descriptors that decide
// whether a family's four sets are present and which asset each resolves to.
type fontFace struct {
	family string
	key    faceKey
	src    string
}

// cssRule is a declaration block with the prelude that introduced it, at any
// nesting depth (a rule inside @media is still a rule), plus the nearest
// enclosing at-rule. That last field is what makes a CONDITIONAL rule
// recognisable: inside @media its own prelude is an ordinary selector, so
// nothing in the rule itself says its applicability is an environment question.
type cssRule struct {
	prelude string
	body    string
	at      string
}

func main() {
	// ContinueOnError: the default ExitOnError collapses -h and a parse error
	// into the same status 2 with no line distinguishing them.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageDoc)
		flag.PrintDefaults()
	}
	cell := flag.String("cell", "", "path to the overlay release's cell.json")
	css := flag.String("css", "", "path to the served CSS bundle built from the pinned UI package")
	fonts := flag.String("fonts", "", "path to the served fonts directory")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, usageErrMsg)
		os.Exit(2)
	}
	if *cell == "" || *css == "" || *fonts == "" {
		fmt.Fprintln(os.Stderr, usageErrMsg)
		os.Exit(2)
	}
	os.Exit(run(*cell, *css, *fonts, os.Stdout, os.Stderr))
}

// run performs the cell-contract gate and returns the process exit code main
// hands to os.Exit: 0 the contract holds, 1 violated, 2 usage error. All three
// inputs are read here rather than in main so a missing file reports as the
// usage error it is (exit 2) instead of a contract verdict (exit 1).
func run(cellPath, cssPath, fontsDir string, stdout, stderr io.Writer) int {
	contract, err := readContract(cellPath)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR cell-contract-gate-usage: cannot read the cell contract at %s: %v\n", cellPath, err)
		fmt.Fprintln(stderr, usageErrMsg)
		return 2
	}
	sheet, err := os.ReadFile(cssPath)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR cell-contract-gate-usage: cannot read the served CSS bundle at %s: %v\n", cssPath, err)
		fmt.Fprintln(stderr, usageErrMsg)
		return 2
	}
	served, err := servedFonts(fontsDir)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR cell-contract-gate-usage: cannot read the served fonts directory at %s: %v\n", fontsDir, err)
		fmt.Fprintln(stderr, usageErrMsg)
		return 2
	}

	if reasons := checkContract(&contract, string(sheet), served); len(reasons) > 0 {
		for _, reason := range reasons {
			fmt.Fprintf(stderr, "ERROR cell-contract-mismatch: %s\n", reason)
		}
		fmt.Fprintln(stderr, remediation())
		return 1
	}
	fmt.Fprintf(stdout, "fontcheck ok: %q leads %q at %gpx/%gpx (ratio %g), four exact faces each\n",
		contract.Family, contract.Companion.Family, contract.Cell.FontSize, contract.Cell.LineHeight, contract.Cell.Ratio)
	return 0
}

// readContract decodes the released cell.json and refuses a document missing
// the fields the gate compares. A contract that cannot state the cell is a
// broken gate input, not a violated contract: there is nothing to compare
// against, so no pin decision follows from it.
func readContract(cellPath string) (cellContract, error) {
	raw, err := os.ReadFile(cellPath)
	if err != nil {
		return cellContract{}, err
	}
	// The release is free to add fields; only the ones read here are a contract
	// with this consumer, so an unknown field must NOT fail the decode.
	var c cellContract
	if err := json.Unmarshal(raw, &c); err != nil {
		return cellContract{}, err
	}
	switch {
	case c.Family == "":
		return cellContract{}, errors.New(`"family" is absent or empty`)
	case c.Companion.Family == "":
		return cellContract{}, errors.New(`"companion.family" is absent or empty`)
	case len(c.Stack) == 0:
		return cellContract{}, errors.New(`"stack" is absent or empty`)
	case c.Cell.FontSize <= 0:
		return cellContract{}, errors.New(`"cell.fontSize" is absent or not positive`)
	case c.Cell.LineHeight <= 0:
		return cellContract{}, errors.New(`"cell.lineHeight" is absent or not positive`)
	case c.Cell.Ratio <= 0:
		return cellContract{}, errors.New(`"cell.ratio" is absent or not positive`)
	}
	return c, nil
}

// servedFonts lists the NAMES of the regular files the image serves beside the
// font, so a @font-face src naming an asset the build no longer fetches is a
// mismatch rather than a runtime 404 nobody sees until a glyph is missing.
//
// It is an inventory and nothing more: it reports that a name is a file, never
// what that file holds. What each file holds is fixed by the Dockerfile's
// sha256 pin on the same asset, which is the whole reason this gate does not
// need to parse one.
func servedFonts(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	served := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			served[e.Name()] = true
		}
	}
	if len(served) == 0 {
		return nil, fmt.Errorf("%s holds no files", dir)
	}
	return served, nil
}

// checkContract returns one reason per violated clause, empty when the
// contract holds. Every clause is a property the overlay's own steering
// records as load-bearing, so each reason names the value it read.
func checkContract(c *cellContract, sheet string, served map[string]bool) []string {
	css := stripComments(sheet)
	rules := parseRules(css)
	var reasons []string

	// The overlay must lead its own declared stack. A release whose stack does
	// not start with its own family has moved the contract, so nothing below
	// can be compared meaningfully.
	if c.Stack[0] != c.Family {
		return []string{fmt.Sprintf("the contract's own stack leads with %q, not the overlay family %q", c.Stack[0], c.Family)}
	}

	matches := rulesFor(rules, termSelector)
	if len(matches) == 0 {
		return append(reasons, "the served CSS declares no `.term` rule, so the cell it renders at cannot be read")
	}

	// Before any value is read: refuse a cascade this reader cannot rank. Every
	// clause below compares ONE declaration per property, which is the computed
	// value only while nothing outranks source order.
	if unresolved := cascadeReasons(css, rules, matches); len(unresolved) > 0 {
		return unresolved
	}
	term := termOwner(matches)

	// The overlay is listed FIRST in font-family, ahead of the companion:
	// the browser takes the tiling codepoints from it and everything else from
	// the text face behind it. Gecko also takes the line box from the first
	// family, which is why the order is load-bearing rather than cosmetic.
	if reason := checkStack(c, rules, term); reason != "" {
		reasons = append(reasons, reason)
	}

	// The cell itself: the two CSS numbers the glyphs were drawn for, and their
	// ratio. The em ADVANCE those numbers were chosen against is the font's own
	// and is checked where the font is, per the package comment.
	if reason := checkCell(c, term); reason != "" {
		reasons = append(reasons, reason)
	}

	// Bold and italic must resolve to a real face. Blink obliques an italic run
	// over the overlay despite its exact italic face, so this declaration is
	// the only thing keeping box drawing upright there.
	if got, ok := declaration(term, "font-synthesis"); !ok || got != "none" {
		reasons = append(reasons, fmt.Sprintf("the served `.term` rule declares font-synthesis %q, not \"none\", so a synthesised stand-in can be drawn over the overlay's own faces", got))
	}

	reasons = append(reasons, checkFaces(c, rules, served)...)

	// The row background is the run padding, never the font's metrics. A
	// re-added override moves the line box the overlay's copied metrics assume,
	// which is the same reflow the cell numbers above exist to catch.
	for _, descriptor := range []string{"ascent-override", "descent-override"} {
		if strings.Contains(css, descriptor) {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares %s, which moves the line box the overlay's copied metrics assume (the row background is the run padding)", descriptor))
		}
	}
	return reasons
}

// checkStack asserts the served --font-mono opens with the contract's stack in
// order, and that .term reads that token rather than a stack of its own.
func checkStack(c *cellContract, rules []cssRule, term cssRule) string {
	if got, ok := declaration(term, "font-family"); !ok || !strings.Contains(got, "var(--font-mono)") {
		return fmt.Sprintf("the served `.term` rule takes its font-family from %q rather than var(--font-mono), so the contract's stack reaches nothing", got)
	}
	value, ok := customProperty(rules, "--font-mono")
	if !ok {
		return "the served CSS declares no --font-mono, so the overlay is in no font-family stack at all"
	}
	families := parseFamilies(value)
	if len(families) < len(c.Stack) {
		return fmt.Sprintf("the served --font-mono is %q, which is shorter than the contract's stack %v", value, c.Stack)
	}
	for i, want := range c.Stack {
		if families[i] != want {
			return fmt.Sprintf("the served --font-mono is %q, whose family %d is %q where the contract's stack requires %q", value, i+1, families[i], want)
		}
	}
	return ""
}

// checkCell asserts the served cell is the one the glyphs were drawn for. The
// two absolute numbers are what pin the CSS; the ratio clause is the released
// contract's own self-consistency, cheap to re-assert here and the one thing
// that catches a release whose ratio was edited without its numbers (the
// glyphs are correct at any size SHARING the ratio, so the ratio is the field
// a second cell target would move).
func checkCell(c *cellContract, term cssRule) string {
	size, sizeOK := pxDeclaration(term, "font-size")
	height, heightOK := pxDeclaration(term, "line-height")
	switch {
	case !sizeOK:
		return "the served `.term` rule declares no font-size in px, so the cell width cannot be compared with the contract"
	case !heightOK:
		return "the served `.term` rule declares no line-height in px, so the cell height cannot be compared with the contract"
	case size != c.Cell.FontSize || height != c.Cell.LineHeight:
		return fmt.Sprintf("the served cell is %gpx/%gpx where the contract declares %gpx/%gpx", size, height, c.Cell.FontSize, c.Cell.LineHeight)
	case math.Abs(height/size-c.Cell.Ratio) > ratioTolerance:
		return fmt.Sprintf("the served cell's ratio is %g where the contract declares %g", height/size, c.Cell.Ratio)
	}
	return ""
}

// checkFaces asserts both families declare all four exact descriptor sets,
// that the overlay's four resolve to ONE asset, and that every asset named is
// one the image actually serves.
func checkFaces(c *cellContract, rules []cssRule, served map[string]bool) []string {
	faces := parseFontFaces(rules)
	overlay := facesByKey(faces, c.Family)
	reasons := checkFamilyFaces(c.Family, overlay, served)
	reasons = append(reasons, checkOverlayIsOneAsset(c.Family, overlay)...)
	return append(reasons, checkFamilyFaces(c.Companion.Family, facesByKey(faces, c.Companion.Family), served)...)
}

// facesByKey groups one family's declared sources by descriptor set.
func facesByKey(faces []fontFace, family string) map[faceKey][]string {
	byKey := map[faceKey][]string{}
	for _, f := range faces {
		if f.family == family {
			byKey[f.key] = append(byKey[f.key], f.src)
		}
	}
	return byKey
}

// checkFamilyFaces asserts one family declares each of the four descriptor sets
// exactly once, against an asset the image serves.
func checkFamilyFaces(family string, byKey map[faceKey][]string, served map[string]bool) []string {
	var reasons []string
	for _, key := range descriptorPairs {
		srcs := byKey[key]
		if len(srcs) == 0 {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares no @font-face for %q at weight %s style %s, so that run resolves to a synthesised or fallback face", family, key.weight, key.style))
			continue
		}
		if len(srcs) > 1 {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares %d @font-face rules for %q at weight %s style %s, so which face wins is an implementation tie", len(srcs), family, key.weight, key.style))
		}
		for _, src := range srcs {
			if asset := path.Base(src); !served[asset] {
				reasons = append(reasons, fmt.Sprintf("the served CSS points %q weight %s style %s at %s, which this image does not serve", family, key.weight, key.style, asset))
			}
		}
	}
	return reasons
}

// checkOverlayIsOneAsset asserts the overlay is ONE asset under four descriptor
// sets. Four distinct assets would mean the family had been rebuilt per style,
// and the generated outlines are upright by design.
func checkOverlayIsOneAsset(family string, byKey map[faceKey][]string) []string {
	distinct := map[string]bool{}
	for _, srcs := range byKey {
		for _, src := range srcs {
			distinct[src] = true
		}
	}
	if len(distinct) > 1 {
		return []string{fmt.Sprintf("the served CSS points %q at %d different assets; the overlay is one upright face under four descriptor sets", family, len(distinct))}
	}
	return nil
}

// remediation names this repo's two pins, which pin to move being build-layout
// knowledge neither the font nor the UI package carries. It also names the case
// where NEITHER pin is the answer, because a reason saying the cascade could not
// be read has a different remedy and an instruction to bump a pin would be
// wrong advice.
func remediation() string {
	return "fix: bump the Dockerfile's WEB_TERMINAL_GLYPHS_VERSION (the contract) or CPLIEGER_WEB_TERMINAL_UI_VERSION (the CSS) so the served cell is the one the glyphs are drawn for" +
		" — unless a reason above says the cascade cannot be ranked, in which case no pin is the answer: the served CSS has to carry one unconditional, unflagged declaration per property, or this gate has to learn to rank the construct"
}

// --- CSS reading -----------------------------------------------------------
//
// A deliberately narrow reader over the bundle this build just produced, not a
// CSS parser: it strips comments, then walks brace depth recording every
// declaration block with the prelude that introduced it and the at-rule
// enclosing it. Comment stripping is load-bearing rather than tidy — the UI's
// own prose mentions `ascent-override`, `line-height: normal` and
// `--font-mono`, so a reader that kept comments would answer from the
// documentation instead of the rules.
//
// Narrow is a licence to REFUSE, never to guess. Reading a value out of this
// structure answers "which declaration comes last", and the browser answers
// "which declaration wins" — the two agree only while importance, layers and
// specificity are all out of play, which is what cascadeReasons establishes
// before any value below is read.

var (
	// A declaration value runs to the first `;` or the end of the block. The
	// property is anchored at a boundary so `font-size` cannot match inside
	// another property's name.
	declPattern = regexp.MustCompile(`(?s)(?:^|[;{\s])([-a-zA-Z]+)\s*:\s*([^;}]*)`)
	// url("x") / url('x') / url(x)
	urlPattern = regexp.MustCompile(`url\(\s*['"]?([^'")]+)['"]?\s*\)`)
	// A declaration's `!important` flag in every spelling CSS allows: the
	// keyword is case-insensitive and whitespace may follow the bang. A flag
	// this reader fails to SEE is a flag it then ranks below source order,
	// which is the whole defect the cascade check exists to close.
	importantPattern = regexp.MustCompile(`(?i)!\s*important`)
)

// stripComments removes every /* ... */ span. CSS comments do not nest, so a
// single pass is exact; an unterminated comment swallows the rest of the file,
// which is what a browser does too.
func stripComments(css string) string {
	var b strings.Builder
	b.Grow(len(css))
	for {
		open := strings.Index(css, "/*")
		if open < 0 {
			b.WriteString(css)
			return b.String()
		}
		b.WriteString(css[:open])
		_, rest, closed := strings.Cut(css[open:], "*/")
		if !closed {
			return b.String()
		}
		// Keep a space so `a/*x*/b` cannot fuse into one token.
		b.WriteByte(' ')
		css = rest
	}
}

// parseRules records every declaration block with its prelude, at any nesting
// depth, so a rule inside @media is found like any other.
func parseRules(css string) []cssRule {
	var rules []cssRule
	var prelude strings.Builder
	depth := 0
	// starts[d] is the offset just past the brace that opened depth d+1.
	var starts []int
	var preludes []string
	for i := range len(css) {
		switch css[i] {
		case '{':
			preludes = append(preludes, strings.TrimSpace(prelude.String()))
			prelude.Reset()
			starts = append(starts, i+1)
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			body := css[starts[depth]:i]
			rules = append(rules, cssRule{prelude: preludes[depth], body: body, at: enclosingAt(preludes[:depth])})
			starts = starts[:depth]
			preludes = preludes[:depth]
			prelude.Reset()
		case ';':
			// A statement at this level ends any prelude accumulated for it
			// (`@import ...;`), so it cannot leak onto the next rule.
			prelude.Reset()
		default:
			prelude.WriteByte(css[i])
		}
	}
	return rules
}

// enclosingAt returns the nearest enclosing at-rule prelude, empty at the top
// level.
func enclosingAt(ancestors []string) string {
	for _, prelude := range slices.Backward(ancestors) {
		if strings.HasPrefix(prelude, "@") {
			return prelude
		}
	}
	return ""
}

// rulesFor returns EVERY rule whose selector list ends in a component carrying
// the given class as a whole token: `.term` is answered by `.term`, by
// `.term:focus` and by the higher-specificity `.term.wt-with-tabbar`, and not
// by `.term-row` (a different class whose name merely starts the same way,
// since `-` continues a CSS identifier).
//
// Matching the compound forms is what lets the cascade check see an override at
// all. A matcher keyed on equality finds the base rule, misses the
// higher-specificity rule that beats it, and reports the loser's value.
func rulesFor(rules []cssRule, class string) []cssRule {
	var found []cssRule
	for _, r := range rules {
		if strings.HasPrefix(r.prelude, "@") {
			continue
		}
		for sel := range strings.SplitSeq(r.prelude, ",") {
			fields := strings.Fields(sel)
			if len(fields) > 0 && hasClassToken(fields[len(fields)-1], class) {
				found = append(found, r)
				break
			}
		}
	}
	return found
}

// hasClassToken reports whether a selector component carries the class as a
// whole token, so `div.term` and `.term:focus` match `.term` and `.term-row`
// does not.
func hasClassToken(component, class string) bool {
	for i := 0; i+len(class) <= len(component); {
		j := strings.Index(component[i:], class)
		if j < 0 {
			return false
		}
		end := i + j + len(class)
		if end == len(component) || !isIdentByte(component[end]) {
			return true
		}
		i = end
	}
	return false
}

// isIdentByte reports whether b can continue a CSS identifier.
func isIdentByte(b byte) bool {
	switch {
	case b == '-' || b == '_':
		return true
	case b >= '0' && b <= '9':
		return true
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	}
	return false
}

// cascadeReasons returns one reason per declaration whose winner this reader
// cannot know, and is empty exactly when source order is the whole answer.
//
// CSS Cascade Level 5 §6 ranks origin and importance FIRST, then cascade
// layers, then selector specificity, and only then order of appearance
// (https://www.w3.org/TR/css-cascade-5/#cascading). This reader implements none
// of that ranking, so "the last rule textually" is the computed value only
// while nothing above order is in play — an `!important` declaration prepended
// to the bundle beats every normal one below it, and a reader answering from
// order reports the loser and approves a build whose terminal renders a
// different cell. Each clause here is therefore a REFUSAL, not a resolution:
// the gate fails closed on a bundle it cannot rank, and teaching it a construct
// is a deliberate change with its own test rather than a silent guess.
func cascadeReasons(css string, rules, matches []cssRule) []string {
	reasons := termCascadeReasons(matches)
	reasons = append(reasons, customPropertyCascadeReasons(rules, fontMonoProperty)...)
	// A cascade layer outranks specificity, so a layered and an unlayered
	// declaration cannot be compared by anything read here. @layer has a
	// statement form (`@layer a, b;`) as well as a block form, so the presence
	// of the at-keyword is the only reliable test; the sheet is already
	// comment-stripped, so this cannot fire on prose.
	if strings.Contains(strings.ToLower(css), "@layer") {
		reasons = append(reasons, "the served CSS declares @layer, and a cascade layer outranks both specificity and source order, so which declaration wins cannot be read from this bundle")
	}
	return reasons
}

// termCascadeReasons refuses a `.term` cell this reader cannot resolve: a
// second rule declaring one of the contract properties (whatever its
// specificity or position), one declared inside a conditional at-rule (whose
// applicability is an environment question, so the served cell is not one
// number), or one flagged `!important`.
func termCascadeReasons(matches []cssRule) []string {
	owners := cellDeclaringRules(matches)
	var reasons []string
	if len(owners) > 1 {
		reasons = append(reasons, fmt.Sprintf("the served CSS declares the %s cell across %d rules (%s), and this reader ranks by source order alone where CSS ranks importance, layers and specificity first — so the cell the browser computes cannot be read from it",
			termSelector, len(owners), strings.Join(preludesOf(owners), " / ")))
	}
	for _, r := range owners {
		if r.at != "" {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares the %s cell inside %s, so whether it applies is an environment question and the served cell is not one number", termSelector, r.at))
		}
		for _, property := range contractProperties {
			if value, ok := declaration(r, property); ok && importantPattern.MatchString(value) {
				reasons = append(reasons, fmt.Sprintf("the served CSS declares %s as %q on the %s cell, and an important declaration outranks every normal one whatever its position, so this reader cannot rank the bundle", property, strings.TrimSpace(value), termSelector))
			}
		}
	}
	return reasons
}

// customPropertyCascadeReasons applies the same refusal one rung up: the
// contract's stack reaches `.term` through a custom property, which the browser
// resolves by the same cascade, so a second declaration of it is the same hole.
func customPropertyCascadeReasons(rules []cssRule, name string) []string {
	var owners []cssRule
	for _, r := range rules {
		if strings.HasPrefix(r.prelude, "@") {
			continue
		}
		if _, ok := declaration(r, name); ok {
			owners = append(owners, r)
		}
	}
	var reasons []string
	if len(owners) > 1 {
		reasons = append(reasons, fmt.Sprintf("the served CSS declares %s in %d rules (%s), so which stack reaches the terminal cannot be read from source order",
			name, len(owners), strings.Join(preludesOf(owners), " / ")))
	}
	for _, r := range owners {
		if r.at != "" {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares %s inside %s, so which stack reaches the terminal is an environment question", name, r.at))
		}
		if value, _ := declaration(r, name); importantPattern.MatchString(value) {
			reasons = append(reasons, fmt.Sprintf("the served CSS declares %s as important, which outranks every normal declaration whatever its position, so this reader cannot rank the bundle", name))
		}
	}
	return reasons
}

// cellDeclaringRules returns the rules declaring at least one of the cell's
// contract properties.
func cellDeclaringRules(matches []cssRule) []cssRule {
	var owners []cssRule
	for _, r := range matches {
		for _, property := range contractProperties {
			if _, ok := declaration(r, property); ok {
				owners = append(owners, r)
				break
			}
		}
	}
	return owners
}

// preludesOf names rules in a diagnostic, so a refusal points at the two rules
// that disagree rather than at the property alone.
func preludesOf(rules []cssRule) []string {
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.prelude)
	}
	return names
}

// termOwner is the rule the cell is read from: the one rule declaring the
// contract properties, which cascadeReasons has already proved is the only one.
// With no declaring rule at all it is the last matching rule, so the clauses
// below report the missing declaration by name instead of reporting nothing.
func termOwner(matches []cssRule) cssRule {
	if owners := cellDeclaringRules(matches); len(owners) == 1 {
		return owners[0]
	}
	return matches[len(matches)-1]
}

// declaration returns a property's value from a rule body, last wins.
func declaration(rule cssRule, property string) (string, bool) {
	value := ""
	ok := false
	for _, m := range declPattern.FindAllStringSubmatch(rule.body, -1) {
		if m[1] == property {
			value, ok = strings.TrimSpace(m[2]), true
		}
	}
	return value, ok
}

// pxDeclaration reads a property whose value must be a px length.
func pxDeclaration(rule cssRule, property string) (float64, bool) {
	raw, ok := declaration(rule, property)
	if !ok || !strings.HasSuffix(raw, "px") {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(raw, "px"), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// customProperty returns a custom property's value from any non-at-rule, last
// wins. Taking the last is safe only because cascadeReasons has already refused
// a bundle declaring it more than once: there is exactly one, and "last" is it.
func customProperty(rules []cssRule, name string) (string, bool) {
	value := ""
	ok := false
	for _, r := range rules {
		if strings.HasPrefix(r.prelude, "@") {
			continue
		}
		if v, found := declaration(r, name); found {
			value, ok = v, true
		}
	}
	return value, ok
}

// parseFamilies splits a font-family value into its family names, dropping the
// quoting a browser drops.
func parseFamilies(value string) []string {
	parts := strings.Split(value, ",")
	families := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		name = strings.Trim(name, `"'`)
		if name != "" {
			families = append(families, name)
		}
	}
	return families
}

// parseFontFaces reduces every @font-face rule to the descriptors this gate
// compares. A rule missing any of the three is skipped rather than reported:
// the per-family completeness check is what names the gap, and it names it
// against the descriptor set the contract requires rather than against a rule
// the reader failed to understand.
func parseFontFaces(rules []cssRule) []fontFace {
	var faces []fontFace
	for _, r := range rules {
		if !strings.HasPrefix(r.prelude, "@font-face") {
			continue
		}
		family, familyOK := declaration(r, "font-family")
		weight, weightOK := declaration(r, "font-weight")
		style, styleOK := declaration(r, "font-style")
		src, srcOK := declaration(r, "src")
		if !familyOK || !weightOK || !styleOK || !srcOK {
			continue
		}
		m := urlPattern.FindStringSubmatch(src)
		if m == nil {
			continue
		}
		faces = append(faces, fontFace{
			family: strings.Trim(strings.TrimSpace(family), `"'`),
			key:    faceKey{weight: strings.TrimSpace(weight), style: strings.TrimSpace(style)},
			src:    filepath.ToSlash(m[1]),
		})
	}
	return faces
}
