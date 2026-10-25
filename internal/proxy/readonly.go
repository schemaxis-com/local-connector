package proxy

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The reasons validateReadOnly refuses a statement. Each one is wrapped, so a
// caller or a test can tell them apart with errors.Is.
var (
	errNotAQuery   = errors.New("only SELECT and WITH statements are allowed")
	errSemicolon   = errors.New("the statement contains a semicolon, and only a single query with no PL/SQL is allowed")
	errInlinePLSQL = errors.New("PL/SQL declared inside a query (WITH FUNCTION or WITH PROCEDURE) is not allowed")
	errForUpdate   = errors.New("SELECT ... FOR UPDATE locks rows and is not allowed")
	errNextval     = errors.New("NEXTVAL advances a sequence and is not allowed")
	errUnreadable  = errors.New("the statement could not be checked")
)

// validateReadOnly refuses a statement unless it is a single query that begins
// as a read and uses none of the forms that let an Oracle query change
// something:
//
//   - WITH FUNCTION or WITH PROCEDURE, PL/SQL declared inside the query, which
//     can run DDL and DML in an autonomous transaction;
//   - FOR UPDATE, which locks the rows it reads;
//   - NEXTVAL, which advances a sequence;
//   - a semicolon outside a literal, which means a second statement or the body
//     of inline PL/SQL.
//
// It reads the statement with a lexer, so a keyword inside a string literal, a
// quoted identifier or a comment is neither matched by mistake nor able to hide
// one. It refuses what it cannot read: an unclosed literal or comment, or any
// character outside ASCII that is not inside a literal, a quoted identifier or
// a comment.
//
// Scope: the check works on the statement's text. Functions a query calls run
// under the privileges of the database account the agent connects as, so a
// dedicated read-only account (see INSTALL.md) extends the same protection to
// them at the database level.
func validateReadOnly(sqlText string) error {
	toks, err := lexSQL(sqlText)
	if err != nil {
		return fmt.Errorf("read-only agent: %w: %w", errUnreadable, err)
	}
	if len(toks) == 0 {
		return fmt.Errorf("read-only agent: %w", errNotAQuery)
	}
	if first := toks[0]; !first.is("SELECT") && !first.is("WITH") {
		if first.kind == tokWord {
			return fmt.Errorf("read-only agent: %w, and this one starts with %s", errNotAQuery, first.text)
		}
		return fmt.Errorf("read-only agent: %w", errNotAQuery)
	}
	for i, t := range toks {
		var next token
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		switch {
		case t.kind == tokPunct && t.text == ";":
			return fmt.Errorf("read-only agent: %w", errSemicolon)
		case t.is("WITH") && (next.is("FUNCTION") || next.is("PROCEDURE")):
			return fmt.Errorf("read-only agent: %w", errInlinePLSQL)
		case t.is("FOR") && next.is("UPDATE"):
			return fmt.Errorf("read-only agent: %w", errForUpdate)
		case t.is("NEXTVAL"), t.kind == tokQuotedIdent && strings.EqualFold(t.text, "NEXTVAL"):
			// A quoted "NEXTVAL" still names the pseudocolumn: Oracle itself
			// writes identity defaults as "OWNER"."SEQ"."NEXTVAL".
			return fmt.Errorf("read-only agent: %w", errNextval)
		}
	}
	return nil
}

type tokenKind int

const (
	tokWord        tokenKind = iota // unquoted identifier, keyword or number, upper-cased
	tokQuotedIdent                  // "quoted identifier": a name, never a keyword
	tokLiteral                      // string literal: its content is data
	tokPunct                        // any other single ASCII character
)

type token struct {
	kind tokenKind
	text string
}

// is reports whether t is the unquoted keyword kw, which must be upper case.
func (t token) is(kw string) bool { return t.kind == tokWord && t.text == kw }

// lexSQL splits an Oracle SQL statement into tokens and drops whitespace and
// comments. Optimizer hints are comments, so they are dropped too.
//
// Outside literals, quoted identifiers and comments it accepts ASCII only.
// Keywords are ASCII, and refusing everything else there means no look-alike
// letter or unusual space can separate or disguise one.
func lexSQL(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		rest := s[i:]
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case strings.HasPrefix(rest, "--"):
			// Ends at the first line break of either kind, so a lone carriage
			// return cannot keep hiding text that the database reads as code.
			if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
				i += end + 1
			} else {
				i = len(s)
			}
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return nil, errors.New("a comment is not closed")
			}
			i += 2 + end + 2
		case c == '\'':
			n, err := literalLen(rest)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokLiteral})
			i += n
		case c == '"':
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return nil, errors.New("a quoted identifier is not closed")
			}
			toks = append(toks, token{kind: tokQuotedIdent, text: rest[1 : 1+end]})
			i += 1 + end + 1
		case isWordByte(c):
			n := 1
			for n < len(rest) && isWordByte(rest[n]) {
				n++
			}
			word := strings.ToUpper(rest[:n])
			// q'[...]' and nq'[...]': Oracle's alternative quoting, where a
			// single quote inside the delimiters does not end the literal.
			if (word == "Q" || word == "NQ") && n < len(rest) && rest[n] == '\'' {
				m, err := alternativeLiteralLen(rest[n:])
				if err != nil {
					return nil, err
				}
				toks = append(toks, token{kind: tokLiteral})
				i += n + m
				continue
			}
			toks = append(toks, token{kind: tokWord, text: word})
			i += n
		case c > ' ' && c < utf8.RuneSelf && c != 0x7f:
			toks = append(toks, token{kind: tokPunct, text: string(c)})
			i++
		default:
			r, _ := utf8.DecodeRuneInString(rest)
			return nil, fmt.Errorf("it contains the character %U outside a literal", r)
		}
	}
	return toks, nil
}

func isWordByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '_' || c == '$' || c == '#'
}

// literalLen returns the length of the '...' literal s starts with, where two
// single quotes in a row stand for one.
func literalLen(s string) (int, error) {
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			i++
			continue
		}
		return i + 1, nil
	}
	return 0, errors.New("a string literal is not closed")
}

// alternativeLiteralLen returns the length of the part of a q-quoted literal
// that follows the q, s starting at its opening single quote. The character
// after that quote opens the literal, and the literal ends at its closing
// counterpart followed by a single quote.
func alternativeLiteralLen(s string) (int, error) {
	open, size := utf8.DecodeRuneInString(s[1:])
	if size == 0 || open == utf8.RuneError || open == '\'' ||
		open == ' ' || open == '\t' || open == '\n' || open == '\r' {
		return 0, errors.New("a q-quoted literal has no valid delimiter")
	}
	closeRune := open
	switch open {
	case '[':
		closeRune = ']'
	case '{':
		closeRune = '}'
	case '<':
		closeRune = '>'
	case '(':
		closeRune = ')'
	}
	body := 1 + size
	end := strings.Index(s[body:], string(closeRune)+"'")
	if end < 0 {
		return 0, errors.New("a q-quoted literal is not closed")
	}
	return body + end + utf8.RuneLen(closeRune) + 1, nil
}
