package importers

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// The Corefile reader implements the server-block grammar that CoreDNS
// documents (https://coredns.io/manual/configuration/), written from that
// description rather than from CoreDNS or Caddy code (D392):
//
//   - Tokens are separated by spaces, tabs and line breaks. A double-quoted
//     token may contain whitespace and line breaks; \" escapes a quote inside.
//   - An unquoted # starts a comment that ends at the line break.
//   - A server block is one or more keys (zone[:port], optionally with a
//     transport scheme such as dns:// or tls://, separated by spaces or
//     commas) followed by { on the same line and closed by a matching }.
//     A file with a single server block may omit the braces.
//   - Inside a block every directive starts a line; its arguments follow on
//     the same line. A trailing { opens a directive sub-block.
//   - (name) { ... } at the top level defines a snippet that import name
//     expands in place. {$NAME} and {%NAME%} are environment placeholders.
//
// The reader never opens files, expands globs or reads the environment.

const (
	maxCorefileTokens = 200000
	maxCorefileDepth  = 8
	maxCorefileBlocks = 1024
)

type cfToken struct {
	text      string
	line, end int
	quoted    bool
}

// placeholder reports an unresolved environment or snippet-argument value.
func (t cfToken) placeholder() bool {
	return strings.Contains(t.text, "{$") || strings.Contains(t.text, "{%") || strings.Contains(t.text, "{args[")
}

func (t cfToken) is(s string) bool { return !t.quoted && t.text == s }

type cfDirective struct {
	name     cfToken
	args     []cfToken
	block    []*cfDirective
	hasBlock bool
}

type cfServerBlock struct {
	keys       []cfToken
	directives []*cfDirective
}

type corefile struct {
	blocks []*cfServerBlock
	// unresolved counts imports that name files, take arguments or recurse.
	unresolved int
	// topImports counts top-level imports, which would add server blocks.
	topImports int
}

func lexCorefile(ctx context.Context, src string) ([]cfToken, error) {
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return nil, errors.New("corefile must be UTF-8 text")
	}
	var out []cfToken
	var b strings.Builder
	line, start := 1, 0
	inToken, quoted, comment := false, false, false
	emit := func() error {
		if !inToken {
			return nil
		}
		if b.Len() > 65536 {
			return errors.New("corefile token exceeds 64 KiB")
		}
		if len(out) >= maxCorefileTokens {
			return errors.New("corefile exceeds 200,000 tokens")
		}
		out = append(out, cfToken{text: b.String(), line: start, end: line, quoted: quoted})
		b.Reset()
		inToken, quoted = false, false
		return nil
	}
	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ch := runes[i]
		if comment {
			if ch == '\n' {
				comment = false
				line++
			}
			continue
		}
		if quoted {
			switch {
			case ch == '\\' && i+1 < len(runes) && runes[i+1] == '"':
				b.WriteRune('"')
				i++
			case ch == '"':
				if err := emit(); err != nil {
					return nil, err
				}
			default:
				if ch == '\n' {
					line++
				}
				b.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case ' ', '\t', '\r', '\n':
			if err := emit(); err != nil {
				return nil, err
			}
			if ch == '\n' {
				line++
			}
		case '#':
			if err := emit(); err != nil {
				return nil, err
			}
			comment = true
		case '"':
			if inToken {
				b.WriteRune(ch)
				continue
			}
			inToken, quoted, start = true, true, line
		default:
			if !inToken {
				inToken, start = true, line
			}
			b.WriteRune(ch)
		}
	}
	if quoted {
		return nil, errors.New("corefile contains an unterminated quoted token")
	}
	if err := emit(); err != nil {
		return nil, err
	}
	return out, nil
}

type cfParser struct {
	ctx  context.Context
	toks []cfToken
	pos  int
}

func parseCorefile(ctx context.Context, src string) (*corefile, error) {
	toks, err := lexCorefile(ctx, src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, errors.New("corefile contains no server blocks")
	}
	p := &cfParser{ctx: ctx, toks: toks}
	cf := &corefile{}
	snippets := map[string][]*cfDirective{}
	for p.pos < len(p.toks) {
		first := p.toks[p.pos]
		if first.is("{") || first.is("}") {
			return nil, errors.New("corefile has an unexpected brace outside a server block")
		}
		if name, ok := snippetName(first); ok {
			p.pos++
			if p.pos >= len(p.toks) || !p.toks[p.pos].is("{") || p.toks[p.pos].line != first.end {
				return nil, errors.New("corefile snippet definition must open with { on the same line")
			}
			p.pos++
			body, err := p.directives(true, 1)
			if err != nil {
				return nil, err
			}
			if _, dup := snippets[name]; dup {
				return nil, errors.New("corefile defines a snippet more than once")
			}
			snippets[name] = body
			continue
		}
		if first.is("import") {
			cf.topImports++
			p.pos++
			for p.pos < len(p.toks) && p.toks[p.pos].line == first.end {
				p.pos++
			}
			continue
		}
		if len(cf.blocks) >= maxCorefileBlocks {
			return nil, errors.New("corefile exceeds 1,024 server blocks")
		}
		blk := &cfServerBlock{}
		last := first
		for p.pos < len(p.toks) {
			t := p.toks[p.pos]
			if t.is("{") {
				break
			}
			if len(blk.keys) > 0 && t.line != last.end && !strings.HasSuffix(last.text, ",") {
				break
			}
			if t.is("}") {
				return nil, errors.New("corefile has an unexpected brace outside a server block")
			}
			blk.keys = append(blk.keys, t)
			last = t
			p.pos++
		}
		switch {
		case p.pos < len(p.toks) && p.toks[p.pos].is("{"):
			p.pos++
			body, err := p.directives(true, 1)
			if err != nil {
				return nil, err
			}
			blk.directives = body
		case len(cf.blocks) == 0 && len(snippets) == 0 && cf.topImports == 0:
			// The documented braceless form: one server block whose keys
			// are the first line and whose body is the rest of the file.
			body, err := p.directives(false, 1)
			if err != nil {
				return nil, err
			}
			blk.directives = body
		default:
			return nil, errors.New("corefile server block keys must be followed by {")
		}
		cf.blocks = append(cf.blocks, blk)
	}
	if len(cf.blocks) == 0 {
		return nil, errors.New("corefile contains no server blocks")
	}
	expanded := 0
	for _, blk := range cf.blocks {
		out, err := cf.expand(ctx, blk.directives, snippets, map[string]bool{}, 0, &expanded)
		if err != nil {
			return nil, err
		}
		blk.directives = out
	}
	return cf, nil
}

func snippetName(t cfToken) (string, bool) {
	if t.quoted || len(t.text) < 3 || t.text[0] != '(' || t.text[len(t.text)-1] != ')' {
		return "", false
	}
	return t.text[1 : len(t.text)-1], true
}

// directives reads directive lines until the closing brace of the current
// block (or the end of input for a braceless file).
func (p *cfParser) directives(closed bool, depth int) ([]*cfDirective, error) {
	if depth > maxCorefileDepth {
		return nil, errors.New("corefile exceeds the nesting limit")
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	var out []*cfDirective
	for {
		if p.pos >= len(p.toks) {
			if closed {
				return nil, errors.New("corefile has an unclosed block")
			}
			return out, nil
		}
		t := p.toks[p.pos]
		if t.is("}") {
			if !closed {
				return nil, errors.New("corefile has an unexpected closing brace")
			}
			p.pos++
			return out, nil
		}
		if t.is("{") {
			return nil, errors.New("corefile has a brace without a directive")
		}
		d := &cfDirective{name: t}
		p.pos++
		last := t
		for p.pos < len(p.toks) && p.toks[p.pos].line == last.end {
			a := p.toks[p.pos]
			if a.is("}") {
				break
			}
			if a.is("{") {
				p.pos++
				if p.pos < len(p.toks) && p.toks[p.pos].line == a.end && !p.toks[p.pos].is("}") {
					return nil, errors.New("corefile directive block must start on a new line after {")
				}
				body, err := p.directives(true, depth+1)
				if err != nil {
					return nil, err
				}
				d.block, d.hasBlock = body, true
				break
			}
			d.args = append(d.args, a)
			last = a
			p.pos++
		}
		out = append(out, d)
	}
}

// expand replaces import lines that name a defined snippet. Imports of
// files or globs, imports with arguments and recursive imports stay
// unresolved; the converter reports them without reading anything.
func (cf *corefile) expand(ctx context.Context, in []*cfDirective, snippets map[string][]*cfDirective, active map[string]bool, depth int, count *int) ([]*cfDirective, error) {
	if depth > maxCorefileDepth {
		return nil, errors.New("corefile snippet imports exceed the nesting limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []*cfDirective
	for _, d := range in {
		if d.name.is("import") {
			*count++
			if *count > maxCorefileTokens {
				return nil, errors.New("corefile expands to more than 200,000 directives")
			}
			body, ok := snippets[firstText(d.args)]
			if len(d.args) != 1 || d.hasBlock || !ok || active[d.args[0].text] {
				cf.unresolved++
				continue
			}
			active[d.args[0].text] = true
			expanded, err := cf.expand(ctx, body, snippets, active, depth+1, count)
			delete(active, d.args[0].text)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
			continue
		}
		// Bound the expanded size, not only the import count: one snippet
		// imported many times must not multiply into an unbounded body.
		*count++
		if *count > maxCorefileTokens {
			return nil, errors.New("corefile expands to more than 200,000 directives")
		}
		if d.hasBlock {
			sub, err := cf.expand(ctx, d.block, snippets, active, depth+1, count)
			if err != nil {
				return nil, err
			}
			d = &cfDirective{name: d.name, args: d.args, block: sub, hasBlock: true}
		}
		out = append(out, d)
	}
	return out, nil
}

func firstText(toks []cfToken) string {
	if len(toks) == 0 {
		return ""
	}
	return toks[0].text
}
