package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fixture is a MINIMAL stand-in for the served bundle, not a copy of it:
// the real bytes are gated at build time by the Dockerfile step these tests
// pin, and a 150 KB copy checked in here would go stale on every UI release
// while proving nothing the build does not already prove.
const fixtureCell = `{
  "family": "Web Terminal Glyphs",
  "companion": { "family": "Monaspace Neon NF", "unitsPerEm": 2000, "advance": 1240 },
  "cell": { "fontSize": 14, "lineHeight": 17, "ratio": 1.2142857, "overhang": 72 },
  "stack": ["Web Terminal Glyphs", "Monaspace Neon NF"],
  "generated": ["2500-259F"]
}`

const fixtureCSS = `/* a comment mentioning ascent-override and --font-mono and line-height: normal */
@font-face {
  font-family: "Monaspace Neon NF";
  src: url("/vendor/fonts/MonaspaceNeonNF-Regular.woff2") format("woff2");
  font-weight: 400;
  font-style: normal;
}
@font-face {
  font-family: "Monaspace Neon NF";
  src: url("/vendor/fonts/MonaspaceNeonNF-Bold.woff2") format("woff2");
  font-weight: 700;
  font-style: normal;
}
@font-face {
  font-family: "Monaspace Neon NF";
  src: url("/vendor/fonts/MonaspaceNeonNF-Italic.woff2") format("woff2");
  font-weight: 400;
  font-style: italic;
}
@font-face {
  font-family: "Monaspace Neon NF";
  src: url("/vendor/fonts/MonaspaceNeonNF-BoldItalic.woff2") format("woff2");
  font-weight: 700;
  font-style: italic;
}
@font-face {
  font-family: "Web Terminal Glyphs";
  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 400;
  font-style: normal;
}
@font-face {
  font-family: "Web Terminal Glyphs";
  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 700;
  font-style: normal;
}
@font-face {
  font-family: "Web Terminal Glyphs";
  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 400;
  font-style: italic;
}
@font-face {
  font-family: "Web Terminal Glyphs";
  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 700;
  font-style: italic;
}
.wt-root {
  --font-mono: "Web Terminal Glyphs", "Monaspace Neon NF", monospace;
  --font-ui: "Monaspace Neon NF", monospace;
}
:where(.wt-root) .term {
  font-family: var(--font-mono);
  font-synthesis: none;
  font-size: 14px;
  line-height: 17px;
}
:where(.wt-root) .term-row span {
  padding-block: 1px;
}
@media (hover: none) {
  :where(.wt-root) .term-link {
    text-decoration: underline dotted;
  }
}
`

var fixtureFonts = []string{
	"MonaspaceNeonNF-Regular.woff2",
	"MonaspaceNeonNF-Bold.woff2",
	"MonaspaceNeonNF-Italic.woff2",
	"MonaspaceNeonNF-BoldItalic.woff2",
	"WebTerminalGlyphs.woff2",
}

// fixture writes the three inputs into a temp tree and returns their paths. It
// is a setup helper: it fails inside itself rather than returning an error for
// every call site to check.
func fixture(t *testing.T, cell, css string, fonts []string) (cellPath, cssPath, fontsDir string) {
	t.Helper()
	dir := t.TempDir()
	cellPath = filepath.Join(dir, "cell.json")
	cssPath = filepath.Join(dir, "style.css")
	fontsDir = filepath.Join(dir, "fonts")
	if err := os.WriteFile(cellPath, []byte(cell), 0o600); err != nil {
		t.Fatalf("Setup: write cell.json: %v", err)
	}
	if err := os.WriteFile(cssPath, []byte(css), 0o600); err != nil {
		t.Fatalf("Setup: write style.css: %v", err)
	}
	if err := os.MkdirAll(fontsDir, 0o700); err != nil {
		t.Fatalf("Setup: mkdir fonts: %v", err)
	}
	for _, name := range fonts {
		if err := os.WriteFile(filepath.Join(fontsDir, name), []byte("woff2"), 0o600); err != nil {
			t.Fatalf("Setup: write %s: %v", name, err)
		}
	}
	return cellPath, cssPath, fontsDir
}

// served is the fixture's font set as checkContract wants it.
func served(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestStripComments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		css  string
		want string
	}{
		{name: "removes a span", css: "a{x:1}/* c */b{y:2}", want: "a{x:1} b{y:2}"},
		{name: "removes a multiline span", css: "a{/*\nline\n*/x:1}", want: "a{ x:1}"},
		{name: "does not fuse tokens", css: "a/*x*/b", want: "a b"},
		{name: "unterminated swallows the tail", css: "a{x:1}/* forever", want: "a{x:1}"},
		{name: "leaves comment-free css alone", css: "a{x:1}", want: "a{x:1}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripComments(tc.css); got != tc.want {
				t.Errorf("stripComments(%q) = %q, want %q", tc.css, got, tc.want)
			}
		})
	}
}

// TestStripCommentsIsWhatMakesTheOverrideCheckReal pins the reason comment
// stripping exists: the UI's own prose names ascent-override, so a reader that
// kept comments would answer from the documentation rather than the rules.
func TestStripCommentsIsWhatMakesTheOverrideCheckReal(t *testing.T) {
	t.Parallel()
	if !strings.Contains(fixtureCSS, "ascent-override") {
		t.Fatal("Setup: the fixture no longer mentions ascent-override in a comment, so this case proves nothing")
	}
	if got := stripComments(fixtureCSS); strings.Contains(got, "ascent-override") {
		t.Error("stripComments left ascent-override in place; the override check would fire on prose")
	}
}

func TestParseRules_recordsEveryDepth(t *testing.T) {
	t.Parallel()
	rules := parseRules(stripComments(fixtureCSS))
	var preludes []string
	for _, r := range rules {
		preludes = append(preludes, r.prelude)
	}
	for _, want := range []string{":where(.wt-root) .term", "@media (hover: none)", ":where(.wt-root) .term-link", ".wt-root"} {
		found := false
		for _, got := range preludes {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("parseRules(fixture) recorded no rule with prelude %q; got %v", want, preludes)
		}
	}
}

func TestRulesFor_matchesWholeComponents(t *testing.T) {
	t.Parallel()
	rules := parseRules(stripComments(fixtureCSS))
	matches := rulesFor(rules, ".term")
	if len(matches) != 1 {
		t.Fatalf(`rulesFor(fixture, ".term") returned %d rules, want the one .term rule`, len(matches))
	}
	if !strings.Contains(matches[0].body, "font-synthesis") {
		t.Errorf(`rulesFor(fixture, ".term")[0] body = %q, want the rule declaring font-synthesis`, matches[0].body)
	}
	if got := rulesFor(rules, ".term-row"); len(got) != 0 {
		t.Errorf(`rulesFor(fixture, ".term-row") matched %d rules, but the fixture's selector is ".term-row span" — a whole-component match must not answer a descendant`, len(got))
	}
}

// TestRulesFor_findsTheCompoundAndCompetingForms pins the matcher the cascade
// check depends on. A matcher keyed on equality sees only the base rule, so a
// higher-specificity override is invisible and the reader reports the loser's
// value; one keyed on substring would answer `.term` with `.term-row`, a
// different class.
func TestRulesFor_findsTheCompoundAndCompetingForms(t *testing.T) {
	t.Parallel()
	css := `.term{font-size:14px}` +
		`:where(.wt-root) .term.wt-with-tabbar{font-size:15px}` +
		`div.term:focus{font-size:16px}` +
		`.term-row{font-size:17px}` +
		`.terminal{font-size:18px}` +
		`.term-row span{font-size:19px}`
	got := preludesOf(rulesFor(parseRules(css), ".term"))
	want := []string{".term", ":where(.wt-root) .term.wt-with-tabbar", "div.term:focus"}
	if !slices.Equal(got, want) {
		t.Errorf("rulesFor(compound fixture, \".term\") = %q, want %q", got, want)
	}
}

func TestHasClassToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		component string
		want      bool
	}{
		{component: ".term", want: true},
		{component: "div.term", want: true},
		{component: ".term:focus", want: true},
		{component: ".term.wt-with-tabbar", want: true},
		{component: ".wt-with-tabbar.term", want: true},
		{component: ".term-row", want: false},
		{component: ".term_row", want: false},
		{component: ".term2", want: false},
		{component: ".terminal", want: false},
		{component: "span", want: false},
		{component: ".ter", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.component, func(t *testing.T) {
			t.Parallel()
			if got := hasClassToken(tc.component, ".term"); got != tc.want {
				t.Errorf("hasClassToken(%q, \".term\") = %v, want %v", tc.component, got, tc.want)
			}
		})
	}
}

// TestParseRules_recordsTheEnclosingAtRule pins the field the conditional
// refusal reads: a rule inside @media carries an ordinary selector as its own
// prelude, so without this nothing distinguishes it from an unconditional one.
func TestParseRules_recordsTheEnclosingAtRule(t *testing.T) {
	t.Parallel()
	rules := parseRules("@media (hover: none){.a{x:1}}\n.b{y:2}\n@supports (a:b){@media print{.c{z:3}}}\n")
	got := map[string]string{}
	for _, r := range rules {
		got[r.prelude] = r.at
	}
	want := map[string]string{
		".a":                   "@media (hover: none)",
		".b":                   "",
		".c":                   "@media print",
		"@media (hover: none)": "",
		"@media print":         "@supports (a:b)",
		"@supports (a:b)":      "",
	}
	if !maps.Equal(got, want) {
		t.Errorf("parseRules() prelude -> enclosing at-rule = %v, want %v", got, want)
	}
}

func TestDeclaration(t *testing.T) {
	t.Parallel()
	rule := cssRule{prelude: ".x", body: "font-size: 14px; font-synthesis: none; font-size: 15px"}
	if got, ok := declaration(rule, "font-size"); !ok || got != "15px" {
		t.Errorf("declaration(font-size) = %q, %v, want \"15px\", true (last wins)", got, ok)
	}
	if got, ok := declaration(rule, "font-synthesis"); !ok || got != "none" {
		t.Errorf("declaration(font-synthesis) = %q, %v, want \"none\", true", got, ok)
	}
	if _, ok := declaration(rule, "size"); ok {
		t.Error(`declaration("size") matched inside "font-size"; the property must be anchored at a boundary`)
	}
	if _, ok := declaration(rule, "line-height"); ok {
		t.Error(`declaration("line-height") reported a property the rule does not declare`)
	}
}

func TestPxDeclaration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		want   float64
		wantOK bool
	}{
		{name: "integer px", body: "line-height: 17px", want: 17, wantOK: true},
		{name: "fractional px", body: "line-height: 17.5px", want: 17.5, wantOK: true},
		{name: "unitless is refused", body: "line-height: 1.2", wantOK: false},
		{name: "rem is refused", body: "line-height: 1.2rem", wantOK: false},
		{name: "normal is refused", body: "line-height: normal", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := pxDeclaration(cssRule{prelude: ".x", body: tc.body}, "line-height")
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("pxDeclaration(%q) = %v, %v, want %v, %v", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseFamilies(t *testing.T) {
	t.Parallel()
	got := parseFamilies(`"Web Terminal Glyphs", 'Monaspace Neon NF' , monospace`)
	want := []string{"Web Terminal Glyphs", "Monaspace Neon NF", "monospace"}
	if len(got) != len(want) {
		t.Fatalf("parseFamilies() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseFamilies()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseFontFaces(t *testing.T) {
	t.Parallel()
	faces := parseFontFaces(parseRules(stripComments(fixtureCSS)))
	if len(faces) != 8 {
		t.Fatalf("parseFontFaces(fixture) returned %d faces, want 8 (four per family)", len(faces))
	}
	overlay := 0
	for _, f := range faces {
		if f.family != "Web Terminal Glyphs" {
			continue
		}
		overlay++
		if f.src != "/vendor/fonts/WebTerminalGlyphs.woff2" {
			t.Errorf("parseFontFaces() overlay face %v src = %q, want /vendor/fonts/WebTerminalGlyphs.woff2", f.key, f.src)
		}
	}
	if overlay != 4 {
		t.Errorf("parseFontFaces(fixture) returned %d overlay faces, want 4", overlay)
	}
}

// TestParseFontFaces_skipsAnIncompleteRule pins that an unreadable rule is
// skipped rather than reported: the per-family completeness check is what names
// the gap, against the descriptor set the contract requires.
func TestParseFontFaces_skipsAnIncompleteRule(t *testing.T) {
	t.Parallel()
	css := `@font-face{font-family:"X";font-weight:400;font-style:normal}` +
		`@font-face{font-family:"X";src:local(X);font-weight:400;font-style:normal}`
	if faces := parseFontFaces(parseRules(css)); len(faces) != 0 {
		t.Errorf("parseFontFaces(incomplete rules) = %v, want none (no src, and a src with no url())", faces)
	}
}

func TestReadContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "the real shape decodes", body: fixtureCell},
		{name: "an added field is tolerated", body: `{"family":"F","companion":{"family":"C"},"cell":{"fontSize":14,"lineHeight":17,"ratio":1.21},"stack":["F"],"somethingNew":42}`},
		{name: "malformed json", body: `{`, wantErr: "unexpected end"},
		{name: "no family", body: `{"companion":{"family":"C"},"cell":{"fontSize":14,"lineHeight":17,"ratio":1.21},"stack":["F"]}`, wantErr: `"family" is absent`},
		{name: "no companion family", body: `{"family":"F","cell":{"fontSize":14,"lineHeight":17,"ratio":1.21},"stack":["F"]}`, wantErr: `"companion.family" is absent`},
		{name: "no stack", body: `{"family":"F","companion":{"family":"C"},"cell":{"fontSize":14,"lineHeight":17,"ratio":1.21}}`, wantErr: `"stack" is absent`},
		{name: "no fontSize", body: `{"family":"F","companion":{"family":"C"},"cell":{"lineHeight":17,"ratio":1.21},"stack":["F"]}`, wantErr: `"cell.fontSize" is absent`},
		{name: "no lineHeight", body: `{"family":"F","companion":{"family":"C"},"cell":{"fontSize":14,"ratio":1.21},"stack":["F"]}`, wantErr: `"cell.lineHeight" is absent`},
		{name: "no ratio", body: `{"family":"F","companion":{"family":"C"},"cell":{"fontSize":14,"lineHeight":17},"stack":["F"]}`, wantErr: `"cell.ratio" is absent`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cell.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("Setup: write cell.json: %v", err)
			}
			_, err := readContract(path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("readContract(%s) = error %v, want nil", tc.name, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("readContract(%s) = nil error, want one naming %q", tc.name, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("readContract(%s) = error %q, want it to name %q", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestReadContract_absentFile(t *testing.T) {
	t.Parallel()
	if _, err := readContract(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("readContract(absent path) = nil error, want one")
	}
}

// TestCheckContract is the verdict table. The control case runs FIRST and is
// fatal: with a fixture the gate rejects, every mutation below would "fail"
// for the wrong reason and the whole table would be vacuous.
func TestCheckContract(t *testing.T) {
	t.Parallel()
	contract, err := readContract(mustWrite(t, fixtureCell))
	if err != nil {
		t.Fatalf("Setup: readContract(fixture) = %v, want nil", err)
	}
	if reasons := checkContract(&contract, fixtureCSS, served(fixtureFonts)); len(reasons) != 0 {
		t.Fatalf("Setup: checkContract(fixture) = %v, want no reasons; every case below would be vacuous", reasons)
	}

	cases := []struct {
		name string
		// mutate returns the contract, CSS and served set for this case.
		mutate func(cellContract, string, []string) (cellContract, string, []string)
		want   string
	}{
		{
			name: "the contract's cell moved",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				c.Cell.LineHeight = 18
				return c, css, fonts
			},
			want: "the served cell is 14px/17px where the contract declares 14px/18px",
		},
		{
			name: "the served cell moved",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, "font-size: 14px", "font-size: 15px", 1), fonts
			},
			want: "the served cell is 15px/17px where the contract declares 14px/17px",
		},
		{
			name: "the contract's ratio disagrees with its own numbers",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				c.Cell.Ratio = 1.5
				return c, css, fonts
			},
			want: "ratio is 1.2142857142857142 where the contract declares 1.5",
		},
		{
			name: "the overlay stopped leading the stack",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css,
					`--font-mono: "Web Terminal Glyphs", "Monaspace Neon NF", monospace;`,
					`--font-mono: "Monaspace Neon NF", "Web Terminal Glyphs", monospace;`, 1), fonts
			},
			want: "whose family 1 is",
		},
		{
			name: "the overlay left the stack",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css,
					`--font-mono: "Web Terminal Glyphs", "Monaspace Neon NF", monospace;`,
					`--font-mono: monospace;`, 1), fonts
			},
			want: "which is shorter than the contract's stack",
		},
		{
			name: "--font-mono is gone",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css,
					`--font-mono: "Web Terminal Glyphs", "Monaspace Neon NF", monospace;`, "", 1), fonts
			},
			want: "declares no --font-mono",
		},
		{
			name: "the term rule stopped reading the token",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, "font-family: var(--font-mono);", "font-family: monospace;", 1), fonts
			},
			want: "rather than var(--font-mono)",
		},
		{
			name: "synthesis is back on",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, "font-synthesis: none;", "font-synthesis: weight style;", 1), fonts
			},
			want: `declares font-synthesis "weight style"`,
		},
		{
			name: "an overlay descriptor set is gone",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, `  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 700;
  font-style: italic;`, "", 1), fonts
			},
			want: `no @font-face for "Web Terminal Glyphs" at weight 700 style italic`,
		},
		{
			name: "a companion descriptor set is gone",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, `  src: url("/vendor/fonts/MonaspaceNeonNF-Bold.woff2") format("woff2");
  font-weight: 700;
  font-style: normal;`, "", 1), fonts
			},
			want: `no @font-face for "Monaspace Neon NF" at weight 700 style normal`,
		},
		{
			name: "a descriptor set is declared twice",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, css + `@font-face{font-family:"Web Terminal Glyphs";src:url("/vendor/fonts/WebTerminalGlyphs.woff2");font-weight:400;font-style:normal}`, fonts
			},
			want: "which face wins is an implementation tie",
		},
		{
			name: "the served asset was renamed",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.ReplaceAll(css, "WebTerminalGlyphs.woff2", "WebTerminalGlyphs-v2.woff2"), fonts
			},
			want: "which this image does not serve",
		},
		{
			name: "a companion face was never fetched",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, css, []string{"WebTerminalGlyphs.woff2"}
			},
			want: "MonaspaceNeonNF-Regular.woff2, which this image does not serve",
		},
		{
			name: "the overlay is served as several assets",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				css = strings.Replace(css, `  src: url("/vendor/fonts/WebTerminalGlyphs.woff2") format("woff2");
  font-weight: 700;
  font-style: italic;`, `  src: url("/vendor/fonts/WebTerminalGlyphs-bi.woff2") format("woff2");
  font-weight: 700;
  font-style: italic;`, 1)
				return c, css, append(fonts, "WebTerminalGlyphs-bi.woff2")
			},
			want: "at 2 different assets",
		},
		{
			name: "a metric override came back",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, `font-family: "Monaspace Neon NF";`,
					`font-family: "Monaspace Neon NF"; ascent-override: 99.5%;`, 1), fonts
			},
			want: "declares ascent-override",
		},
		// --- the cascade the reader may not rank ----------------------------
		//
		// Each of these leaves the later `.term` rule the reader used to answer
		// from in place, so a gate keyed on source order reports 14px/17px and
		// exits 0 while the browser computes something else. The first case is
		// the reproduction from the review that filed this.
		{
			name: "an important cell declaration outranks the rule the reader reads",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, `:where(.wt-root) .term { font-size: 15px !important; }` + css, fonts
			},
			want: `declares font-size as "15px !important" on the .term cell`,
		},
		{
			// The ONLY cell rule, flagged, in the spelling a case-sensitive
			// literal search misses: the keyword is case-insensitive and
			// whitespace may follow the bang. One owner, so this case isolates
			// the importance clause from the two-rule clause above.
			name: "the only cell rule is important, in an unusual spelling",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, "font-size: 14px;", "font-size: 14px ! IMPORTANT;", 1), fonts
			},
			want: "an important declaration outranks every normal one",
		},
		{
			name: "a more specific second cell rule",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, css + `:where(.wt-root) .term.wt-with-tabbar { line-height: 18px; }`, fonts
			},
			want: "declares the .term cell across 2 rules (:where(.wt-root) .term / :where(.wt-root) .term.wt-with-tabbar)",
		},
		{
			name: "a conditional cell rule",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, css + `@media (pointer: fine) { :where(.wt-root) .term { line-height: 18px; } }`, fonts
			},
			want: "inside @media (pointer: fine), so whether it applies is an environment question",
		},
		{
			name: "the only cell rule is conditional",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, ":where(.wt-root) .term {", "@media print { :where(.wt-root) .term {", 1) + "}", fonts
			},
			want: "inside @media print",
		},
		{
			name: "a cascade layer is declared",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, "@layer base, overrides;\n" + css, fonts
			},
			want: "declares @layer",
		},
		{
			name: "the stack token is declared twice",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, css + `.wt-root.wt-narrow { --font-mono: monospace; }`, fonts
			},
			want: "declares --font-mono in 2 rules (.wt-root / .wt-root.wt-narrow)",
		},
		{
			name: "the stack token is important",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, ", monospace;", ", monospace !important;", 1), fonts
			},
			want: "declares --font-mono as important",
		},
		{
			// Wrap the token's own rule and nothing else, so exactly one clause
			// fires: with the cell rule left unconditional, removing the
			// custom-property arm alone turns this case green.
			name: "the stack token is conditional",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				css = strings.Replace(css, ".wt-root {\n  --font-mono", "@media (min-width: 40em) {\n.wt-root {\n  --font-mono", 1)
				return c, strings.Replace(css, "}\n:where(.wt-root) .term {", "}\n}\n:where(.wt-root) .term {", 1), fonts
			},
			want: "declares --font-mono inside @media (min-width: 40em)",
		},
		{
			name: "the term rule is gone",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				return c, strings.Replace(css, ":where(.wt-root) .term {", ":where(.wt-root) .term-gone {", 1), fonts
			},
			want: "declares no `.term` rule",
		},
		{
			name: "the contract's stack does not lead with its family",
			mutate: func(c cellContract, css string, fonts []string) (cellContract, string, []string) {
				c.Stack = []string{"Monaspace Neon NF", "Web Terminal Glyphs"}
				return c, css, fonts
			},
			want: "the contract's own stack leads with",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, css, fonts := tc.mutate(contract, fixtureCSS, fixtureFonts)
			reasons := checkContract(&c, css, served(fonts))
			if len(reasons) == 0 {
				t.Fatalf("checkContract(%s) = no reasons, want one naming %q", tc.name, tc.want)
			}
			joined := strings.Join(reasons, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("checkContract(%s) reasons =\n%s\nwant one naming %q", tc.name, joined, tc.want)
			}
		})
	}
}

func mustWrite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cell.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("Setup: write cell.json: %v", err)
	}
	return path
}

// TestRun_exitCodeContract pins the three codes against their causes, because
// the two failures have OPPOSITE remedies: exit 2 means fix the gate and do
// NOT move a pin, exit 1 means a pin or the CSS has to move.
func TestRun_exitCodeContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (cell, css, fonts string)
		want  int
	}{
		{
			name: "the contract holds",
			setup: func(t *testing.T) (string, string, string) {
				return fixture(t, fixtureCell, fixtureCSS, fixtureFonts)
			},
			want: 0,
		},
		{
			name: "the cell moved",
			setup: func(t *testing.T) (string, string, string) {
				return fixture(t, strings.Replace(fixtureCell, `"lineHeight": 17`, `"lineHeight": 18`, 1), fixtureCSS, fixtureFonts)
			},
			want: 1,
		},
		{
			name: "the contract is unreadable",
			setup: func(t *testing.T) (string, string, string) {
				cell, css, fonts := fixture(t, fixtureCell, fixtureCSS, fixtureFonts)
				return filepath.Join(filepath.Dir(cell), "absent.json"), css, fonts
			},
			want: 2,
		},
		{
			name: "the css bundle is unreadable",
			setup: func(t *testing.T) (string, string, string) {
				cell, css, fonts := fixture(t, fixtureCell, fixtureCSS, fixtureFonts)
				return cell, filepath.Join(filepath.Dir(css), "absent.css"), fonts
			},
			want: 2,
		},
		{
			name: "the fonts directory is unreadable",
			setup: func(t *testing.T) (string, string, string) {
				cell, css, fonts := fixture(t, fixtureCell, fixtureCSS, fixtureFonts)
				return cell, css, filepath.Join(fonts, "absent")
			},
			want: 2,
		},
		{
			name: "the fonts directory is empty",
			setup: func(t *testing.T) (string, string, string) {
				return fixture(t, fixtureCell, fixtureCSS, nil)
			},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cell, css, fonts := tc.setup(t)
			var stdout, stderr strings.Builder
			if got := run(cell, css, fonts, &stdout, &stderr); got != tc.want {
				t.Errorf("run(%s) = %d, want %d\nstdout: %s\nstderr: %s", tc.name, got, tc.want, stdout.String(), stderr.String())
			}
		})
	}
}

// TestRun_failureNamesBothPins pins that a violation tells the reader which of
// the two independently-moving pins to consider.
func TestRun_failureNamesBothPins(t *testing.T) {
	t.Parallel()
	cell, css, fonts := fixture(t, strings.Replace(fixtureCell, `"lineHeight": 17`, `"lineHeight": 18`, 1), fixtureCSS, fixtureFonts)
	var stdout, stderr strings.Builder
	if got := run(cell, css, fonts, &stdout, &stderr); got != 1 {
		t.Fatalf("Setup: run(moved cell) = %d, want 1", got)
	}
	for _, pin := range []string{"WEB_TERMINAL_GLYPHS_VERSION", "CPLIEGER_WEB_TERMINAL_UI_VERSION"} {
		if !strings.Contains(stderr.String(), pin) {
			t.Errorf("run(moved cell) stderr does not name %s:\n%s", pin, stderr.String())
		}
	}
}

// --- the Dockerfile step -----------------------------------------------------
//
// run() being correct says nothing about the gate being WIRED. These read the
// shipped Dockerfile's logical lines, because a gate that is commented out, or
// reverted to `go run` (which collapses exit 2 into exit 1, the opposite
// remedy), leaves the suite green and ships a silent reflow.

func dockerfileUnderTest(t *testing.T) []string {
	t.Helper()
	// Two levels up from scripts/fontcheck/, which is where `go test ./...` runs it.
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("Setup: read Dockerfile: %v", err)
	}
	return dockerfileLogicalLines(string(data))
}

// dockerfileLogicalLines folds backslash-continued lines into one logical line
// and drops comment/blank lines: without the fold, a gate split across
// continuations reads as several unrelated lines and every matcher below
// returns false on correct input.
func dockerfileLogicalLines(text string) []string {
	var out []string
	var b strings.Builder
	for raw := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if b.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if cut, ok := strings.CutSuffix(trimmed, `\`); ok {
			b.WriteString(cut)
			b.WriteString(" ")
			continue
		}
		b.WriteString(trimmed)
		out = append(out, b.String())
		b.Reset()
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// gateBuildOutput returns the -o path a logical line builds the gate to, or "".
func gateBuildOutput(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f != "-o" || i+2 >= len(fields) {
			continue
		}
		// The package argument must follow the output path for this to be OUR build.
		for _, later := range fields[i+2:] {
			if strings.HasSuffix(later, "/scripts/fontcheck") || later == "./scripts/fontcheck" {
				return fields[i+1]
			}
		}
	}
	return ""
}

// lineInvokesTheGate reports whether line builds the gate and then RUNS the
// built binary with all three of its arguments.
func lineInvokesTheGate(line string) bool {
	out := gateBuildOutput(line)
	if out == "" {
		return false
	}
	idx := strings.Index(line, "-o "+out)
	if idx < 0 {
		return false
	}
	// Skip past the build's own `-o <out>` argument — otherwise the first
	// occurrence of out (the build target) matches the build itself.
	tail := line[idx+len("-o "+out):]
	runIdx := strings.Index(tail, out)
	if runIdx < 0 {
		return false // the binary is never invoked, only produced
	}
	invocation := tail[runIdx:]
	for _, op := range []string{"||", "&&", ";", "|", "&"} {
		if strings.Contains(invocation, op) {
			return false
		}
	}
	for _, flag := range []string{"-cell ", "-css ", "-fonts "} {
		if !strings.Contains(invocation, flag) {
			return false
		}
	}
	return true
}

// lineRunsTheGateUnbuilt reports whether line reaches the gate through `go
// run`, which discards the exit code the Dockerfile branches on.
func lineRunsTheGateUnbuilt(line string) bool {
	return strings.Contains(line, "go run") &&
		(strings.Contains(line, "./scripts/fontcheck") || strings.Contains(line, "/scripts/fontcheck "))
}

func TestDockerfileInvokesTheGate(t *testing.T) {
	t.Parallel()
	invocations := 0
	for _, line := range dockerfileUnderTest(t) {
		if lineInvokesTheGate(line) {
			invocations++
		}
	}
	if invocations != 1 {
		t.Errorf("Dockerfile builds-and-runs the cell-contract gate %d times, want exactly 1; without it a font or UI bump that moves the cell reflows the terminal with a green build", invocations)
	}
}

func TestDockerfileBuildsTheGateInsteadOfGoRun(t *testing.T) {
	t.Parallel()
	for i, line := range dockerfileUnderTest(t) {
		if lineRunsTheGateUnbuilt(line) {
			t.Errorf("Dockerfile logical line %d reaches the gate through `go run`, which discards its exit code: %s", i, line)
		}
	}
}

func TestDockerfileLogicalLines_foldsAContinuedChain(t *testing.T) {
	t.Parallel()
	got := dockerfileLogicalLines("# c\n\nRUN a && \\\n    b && \\\n    c\nARG X=1\n")
	want := []string{"RUN a &&  b &&  c", "ARG X=1"}
	if len(got) != len(want) {
		t.Fatalf("dockerfileLogicalLines() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dockerfileLogicalLines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineInvokesTheGate_rejectsInertForms(t *testing.T) {
	t.Parallel()
	real := "RUN go build -o /tmp/fontcheck-bin/fontcheck ./scripts/fontcheck && " +
		"/tmp/fontcheck-bin/fontcheck -cell static/vendor/fonts/WebTerminalGlyphs-cell.json " +
		"-css static/style.css -fonts static/vendor/fonts"
	if !lineInvokesTheGate(real) {
		t.Fatalf("Setup: lineInvokesTheGate(the real shape) = false, want true: %s", real)
	}
	cases := []struct {
		name string
		line string
	}{
		{name: "built but never invoked", line: "RUN go build -o /tmp/fontcheck-bin/fontcheck ./scripts/fontcheck"},
		{name: "invoked with no arguments", line: "RUN go build -o /tmp/f ./scripts/fontcheck && /tmp/f"},
		{name: "missing the fonts argument", line: "RUN go build -o /tmp/f ./scripts/fontcheck && /tmp/f -cell c.json -css s.css"},
		{name: "failure swallowed by ||", line: "RUN go build -o /tmp/f ./scripts/fontcheck && /tmp/f -cell c -css s -fonts d || true"},
		{name: "another package built to the same name", line: "RUN go build -o /tmp/fontcheck ./scripts/wirecheck && /tmp/fontcheck -cell c -css s -fonts d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if lineInvokesTheGate(tc.line) {
				t.Errorf("lineInvokesTheGate(%q) = true, want false", tc.line)
			}
		})
	}
}

func TestLineRunsTheGateUnbuilt(t *testing.T) {
	t.Parallel()
	if !lineRunsTheGateUnbuilt("RUN go run ./scripts/fontcheck -cell c -css s -fonts d") {
		t.Error("lineRunsTheGateUnbuilt(`go run ./scripts/fontcheck …`) = false, want true")
	}
	if lineRunsTheGateUnbuilt("RUN go build -o /tmp/f ./scripts/fontcheck && /tmp/f -cell c -css s -fonts d") {
		t.Error("lineRunsTheGateUnbuilt(the built form) = true, want false")
	}
}

// --- the PROCESS exit code ---------------------------------------------------

const gateEnvVar = "FONTCHECK_TEST_RUN_AS_GATE"

func TestMain(m *testing.M) {
	if os.Getenv(gateEnvVar) != "" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runGate(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), gateEnvVar+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Setup: run the gate as a process: %v", err)
	}
	return exitErr.ExitCode(), string(out)
}

// TestGateProcessExitCodes re-enters the test binary as the gate, because a
// main() that collapsed a 2 into a 1 is invisible to an in-process test of run.
func TestGateProcessExitCodes(t *testing.T) {
	t.Parallel()
	cell, css, fonts := fixture(t, fixtureCell, fixtureCSS, fixtureFonts)
	movedCell, _, _ := fixture(t, strings.Replace(fixtureCell, `"lineHeight": 17`, `"lineHeight": 18`, 1), fixtureCSS, fixtureFonts)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "the contract holds", args: []string{"-cell", cell, "-css", css, "-fonts", fonts}, want: 0},
		{name: "the cell moved", args: []string{"-cell", movedCell, "-css", css, "-fonts", fonts}, want: 1},
		{name: "no arguments", args: nil, want: 2},
		{name: "cell only", args: []string{"-cell", cell}, want: 2},
		{name: "an unknown flag", args: []string{"-nope"}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, out := runGate(t, tc.args...)
			if got != tc.want {
				t.Errorf("fontcheck %v exited %d, want %d\n%s", tc.args, got, tc.want, out)
			}
		})
	}
}

// TestGateHelpIsNotAUsageError pins -h apart from a parse error: the default
// flag.ExitOnError collapses both into status 2 with no line distinguishing
// them, so a reader cannot tell "you asked for help" from "the gate is broken".
func TestGateHelpIsNotAUsageError(t *testing.T) {
	t.Parallel()
	if got, out := runGate(t, "-h"); got != 0 {
		t.Errorf("fontcheck -h exited %d, want 0\n%s", got, out)
	}
	if got, out := runGate(t, "-nope"); got != 2 || !strings.Contains(out, usageErrMsg) {
		t.Errorf("fontcheck -nope exited %d without the usage line, want 2 with it\n%s", got, out)
	}
}
