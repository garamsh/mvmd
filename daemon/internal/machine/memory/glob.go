package memory

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// compileGlob returns the matcher of a Search pattern: the Guest Agent's glob,
// globset 0.4.21 with a literal separator, backslash escapes, no empty
// alternates and no unclosed class. It follows globset's parser and its
// translation to a regular expression over bytes, so a construct the two
// would read differently, such as a negated class matching "/", is matched as
// the agent matches it. Match a path with matchGlob.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	p := &globParser{chars: []rune(pattern), prev: none, cur: none, branches: [][]globToken{nil}}
	if err := p.parse(); err != nil {
		return nil, err
	}
	if len(p.branches) > 1 {
		return nil, errors.New("unclosed alternate group; missing '}'")
	}
	return regexp.Compile(globRegexp(p.branches[0]))
}

// matchGlob reports whether the path matches, byte by byte as the agent does:
// "?" and "[!a]" match one byte, so not a character of several bytes.
func matchGlob(re *regexp.Regexp, path string) bool {
	runes := make([]rune, len(path))
	for i := range len(path) {
		runes[i] = rune(path[i])
	}
	return re.MatchString(string(runes))
}

type globTokenKind int

const (
	globLiteral globTokenKind = iota
	globAny
	globZeroOrMore
	globRecursivePrefix
	globRecursiveSuffix
	globRecursiveZeroOrMore
	globClass
	globAlternates
)

type globToken struct {
	kind       globTokenKind
	char       rune
	negated    bool
	ranges     [][2]rune
	alternates [][]globToken
}

// none stands for no character, as the end of the pattern.
const none rune = -1

type globParser struct {
	chars     []rune
	pos       int
	prev, cur rune
	stack     []int
	branches  [][]globToken
}

func (p *globParser) bump() rune {
	p.prev = p.cur
	p.cur = none
	if p.pos < len(p.chars) {
		p.cur = p.chars[p.pos]
		p.pos++
	}
	return p.cur
}

func (p *globParser) peek() rune {
	if p.pos < len(p.chars) {
		return p.chars[p.pos]
	}
	return none
}

func (p *globParser) push(t globToken) {
	last := len(p.branches) - 1
	p.branches[last] = append(p.branches[last], t)
}

func (p *globParser) haveTokens() bool { return len(p.branches[len(p.branches)-1]) > 0 }

func (p *globParser) pop() {
	last := len(p.branches) - 1
	p.branches[last] = p.branches[last][:len(p.branches[last])-1]
}

func (p *globParser) parse() error {
	for c := p.bump(); c != none; c = p.bump() {
		var err error
		switch c {
		case '?':
			p.push(globToken{kind: globAny})
		case '*':
			p.star()
		case '[':
			err = p.class()
		case '{':
			p.stack = append(p.stack, len(p.branches))
			p.branches = append(p.branches, nil)
		case '}':
			err = p.closeAlternates()
		case ',':
			if len(p.stack) == 0 {
				p.push(globToken{kind: globLiteral, char: ','})
			} else {
				p.branches = append(p.branches, nil)
			}
		case '\\':
			if next := p.bump(); next == none {
				err = errors.New("dangling '\\'")
			} else {
				p.push(globToken{kind: globLiteral, char: next})
			}
		default:
			p.push(globToken{kind: globLiteral, char: c})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *globParser) closeAlternates() error {
	if len(p.stack) == 0 {
		return errors.New("unopened alternate group; missing '{'")
	}
	start := p.stack[len(p.stack)-1]
	p.stack = p.stack[:len(p.stack)-1]
	alternates := append([][]globToken(nil), p.branches[start:]...)
	p.branches = p.branches[:start]
	p.push(globToken{kind: globAlternates, alternates: alternates})
	return nil
}

// star parses a "*" or "**" the way globset does: "**" crosses "/" only as a
// whole path segment, and is two "*" anywhere else.
func (p *globParser) star() {
	prev := p.prev
	if p.peek() != '*' {
		p.push(globToken{kind: globZeroOrMore})
		return
	}
	p.bump()
	twoStars := func() {
		p.push(globToken{kind: globZeroOrMore})
		p.push(globToken{kind: globZeroOrMore})
	}
	if !p.haveTokens() {
		if next := p.peek(); next != none && next != '/' {
			twoStars()
		} else {
			p.push(globToken{kind: globRecursivePrefix})
			p.bump()
		}
		return
	}
	if prev != '/' && (len(p.branches) <= 1 || (prev != ',' && prev != '{')) {
		twoStars()
		return
	}
	var suffix bool
	switch next := p.peek(); {
	case next == none:
		p.bump()
		suffix = true
	case (next == ',' || next == '}') && len(p.branches) >= 2:
		suffix = true
	case next == '/':
		p.bump()
	default:
		twoStars()
		return
	}
	last := p.branches[len(p.branches)-1]
	kind := last[len(last)-1].kind
	p.pop()
	switch {
	case kind == globRecursivePrefix || kind == globRecursiveSuffix:
		p.push(globToken{kind: kind})
	case suffix:
		p.push(globToken{kind: globRecursiveSuffix})
	default:
		p.push(globToken{kind: globRecursiveZeroOrMore})
	}
}

// class parses a "[...]" after its "[".
func (p *globParser) class() error {
	var ranges [][2]rune
	negated := false
	if c := p.peek(); c == '!' || c == '^' {
		p.bump()
		negated = true
	}
	setEnd := func(end rune) error {
		r := &ranges[len(ranges)-1]
		r[1] = end
		if r[1] < r[0] {
			return fmt.Errorf("invalid range; %q > %q", r[0], r[1])
		}
		return nil
	}
	first, inRange := true, false
	for {
		c := p.bump()
		if c == none {
			return errors.New("unclosed character class; missing ']'")
		}
		switch {
		case c == ']' && !first:
			if inRange {
				ranges = append(ranges, [2]rune{'-', '-'})
			}
			p.push(globToken{kind: globClass, negated: negated, ranges: ranges})
			return nil
		case c == ']' || (c == '-' && first):
			ranges = append(ranges, [2]rune{c, c})
		case c == '-' && !inRange:
			inRange = true
		default:
			if inRange {
				if err := setEnd(c); err != nil {
					return err
				}
			} else {
				ranges = append(ranges, [2]rune{c, c})
			}
			inRange = false
		}
		first = false
	}
}

// globRegexp translates tokens to a regular expression as globset does.
func globRegexp(tokens []globToken) string {
	if len(tokens) == 1 && tokens[0].kind == globRecursivePrefix {
		return "^.*$"
	}
	return "^" + globTokensRegexp(tokens) + "$"
}

func globTokensRegexp(tokens []globToken) string {
	var re strings.Builder
	for _, t := range tokens {
		switch t.kind {
		case globLiteral:
			re.WriteString(escapeRune(t.char))
		case globAny:
			re.WriteString("[^/]")
		case globZeroOrMore:
			re.WriteString("[^/]*")
		case globRecursivePrefix:
			re.WriteString("(?:/?|.*/)")
		case globRecursiveSuffix:
			re.WriteString("/.*")
		case globRecursiveZeroOrMore:
			re.WriteString("(?:/|/.*/)")
		case globClass:
			re.WriteByte('[')
			if t.negated {
				re.WriteByte('^')
			}
			for _, r := range t.ranges {
				re.WriteString(escapeRune(r[0]))
				if r[0] != r[1] {
					re.WriteString("-" + escapeRune(r[1]))
				}
			}
			re.WriteByte(']')
		case globAlternates:
			var parts []string
			for _, alternate := range t.alternates {
				if part := globTokensRegexp(alternate); part != "" {
					parts = append(parts, part)
				}
			}
			if len(parts) > 0 {
				re.WriteString("(?:" + strings.Join(parts, "|") + ")")
			}
		}
	}
	return re.String()
}

// escapeRune writes each byte of the character as a hex escape, which is
// literal in and out of a class.
func escapeRune(c rune) string {
	var out strings.Builder
	for _, b := range []byte(string(c)) {
		fmt.Fprintf(&out, `\x%02x`, b)
	}
	return out.String()
}
