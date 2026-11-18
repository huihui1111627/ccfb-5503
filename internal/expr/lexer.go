// Package expr 实现 SQL 子集的词法、语法分析与逐行求值。
//
// 支持的子集以窗口查询为中心：算术/比较/逻辑运算、CASE、COALESCE 等标量
// 函数、CAST、interval 字面量，以及窗口函数调用（解析成 AST，由 window 包计算）。
package expr

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenKind 词法单元类别。
type TokenKind int

const (
	TkEOF TokenKind = iota
	TkIdent
	TkInt
	TkFloat
	TkString
	TkOp
)

// Token 一个词法单元。
type Token struct {
	Kind TokenKind
	// 原始文本（字符串字面量已去引号、转义已还原；ident 保留原始大小写）。
	Text string
	Pos  int
}

// lexer 状态机。
type lexer struct {
	src string
	pos int
}

func lex(src string) ([]Token, error) {
	l := &lexer{src: src}
	var toks []Token
	for {
		l.skipSpaceAndComments()
		if l.pos >= len(l.src) {
			toks = append(toks, Token{Kind: TkEOF, Pos: l.pos})
			return toks, nil
		}
		start := l.pos
		c := l.src[l.pos]
		switch {
		case c == '"':
			id, err := l.readQuotedIdent()
			if err != nil {
				return nil, err
			}
			toks = append(toks, Token{Kind: TkIdent, Text: id, Pos: start})
		case c == '\'':
			s, err := l.readString()
			if err != nil {
				return nil, err
			}
			toks = append(toks, Token{Kind: TkString, Text: s, Pos: start})
		case isIdentStart(c):
			for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
				l.pos++
			}
			toks = append(toks, Token{Kind: TkIdent, Text: l.src[start:l.pos], Pos: start})
		case unicode.IsDigit(rune(c)):
			tk, err := l.readNumber()
			if err != nil {
				return nil, err
			}
			toks = append(toks, tk)
		default:
			op, ok := l.readOperator()
			if !ok {
				return nil, fmt.Errorf("lex error at pos %d: unexpected character %q", l.pos, string(c))
			}
			toks = append(toks, Token{Kind: TkOp, Text: op, Pos: start})
		}
	}
}

func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if unicode.IsSpace(rune(c)) {
			l.pos++
			continue
		}
		// -- 行注释
		if c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		// /* 块注释，支持嵌套，与 SQL 一致。
		if c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			depth := 1
			l.pos += 2
			for l.pos < len(l.src) && depth > 0 {
				if l.src[l.pos] == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
					depth++
					l.pos += 2
				} else if l.src[l.pos] == '*' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
					depth--
					l.pos += 2
				} else {
					l.pos++
				}
			}
			if depth > 0 {
				// 未闭合：直接结束，交给后续逻辑报 EOF。
				return
			}
			continue
		}
		return
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

func (l *lexer) readQuotedIdent() (string, error) {
	l.pos++ // 跳过开引号
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '"' {
				b.WriteByte('"')
				l.pos += 2
				continue
			}
			l.pos++
			return b.String(), nil
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", fmt.Errorf("lex error: unterminated quoted identifier")
}

func (l *lexer) readString() (string, error) {
	l.pos++ // 跳过开引号
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\'' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				b.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++
			return b.String(), nil
		}
		if c == '\\' && l.pos+1 < len(l.src) {
			// 支持反斜杠转义的常见形式（PostgreSQL 默认 standard_conforming_strings=on
			// 时反斜杠是普通字符；这里仅在 E'...' 场景需要，普通字符串保持原样）。
			b.WriteByte(c)
			b.WriteByte(l.src[l.pos+1])
			l.pos += 2
			continue
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", fmt.Errorf("lex error: unterminated string literal")
}

func (l *lexer) readNumber() (Token, error) {
	start := l.pos
	isFloat := false
	for l.pos < len(l.src) && unicode.IsDigit(rune(l.src[l.pos])) {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		isFloat = true
		l.pos++
		for l.pos < len(l.src) && unicode.IsDigit(rune(l.src[l.pos])) {
			l.pos++
		}
	}
	// 科学计数法
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		isFloat = true
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		for l.pos < len(l.src) && unicode.IsDigit(rune(l.src[l.pos])) {
			l.pos++
		}
	}
	kind := TkInt
	if isFloat {
		kind = TkFloat
	}
	return Token{Kind: kind, Text: l.src[start:l.pos], Pos: start}, nil
}

// multiCharOps 按长度降序排列以保证最长匹配。
var multiCharOps = []string{
	"::", "<>", ">=", "<=", "!=", "||", "->",
	"+", "-", "*", "/", "%", "=", "<", ">",
	"(", ")", ",", ".", ";",
}

func (l *lexer) readOperator() (string, bool) {
	for _, op := range multiCharOps {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.pos += len(op)
			return op, true
		}
	}
	return "", false
}
