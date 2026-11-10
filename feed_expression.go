package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

const maxFeedBranches = 32

// A clause is one AND branch; the complete expression is the OR of its clauses.
type FeedTagClause struct {
	Tags    []string `json:"tags"`
	Exclude []string `json:"exclude"`
}
type feedTagNode struct {
	op, tag     string
	left, right *feedTagNode
}
type feedTagToken struct {
	kind, value string
	position    int
}
type feedTagParser struct {
	tokens []feedTagToken
	at     int
}

func compileFeedExpression(expression string) ([]FeedTagClause, error) {
	if strings.TrimSpace(expression) == "" {
		return []FeedTagClause{{}}, nil
	}
	if len(expression) > 4096 {
		return nil, fmt.Errorf("выражение тегов слишком длинное (максимум 4096 байт)")
	}
	tokens, err := lexFeedExpression(expression)
	if err != nil {
		return nil, err
	}
	parser := feedTagParser{tokens: tokens}
	node, err := parser.or()
	if err != nil {
		return nil, err
	}
	if parser.current().kind != "end" {
		return nil, parser.error("лишний символ или оператор")
	}
	return feedTagBranches(node, false)
}
func lexFeedExpression(expression string) ([]feedTagToken, error) {
	chars := []rune(expression)
	var tokens []feedTagToken
	for i := 0; i < len(chars); {
		if unicode.IsSpace(chars[i]) {
			i++
			continue
		}
		start := i
		token := feedTagToken{kind: "tag", position: i + 1}
		switch chars[i] {
		case '(', ')':
			token.kind = string(chars[i])
			i++
		case '"':
			i++
			for i < len(chars) && chars[i] != '"' {
				if chars[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(chars) {
				return nil, fmt.Errorf("теги: незакрытые кавычки, позиция %d", start+1)
			}
			i++
			if err := json.Unmarshal([]byte(string(chars[start:i])), &token.value); err != nil {
				return nil, fmt.Errorf("теги: неверные кавычки или экранирование, позиция %d", start+1)
			}
		default:
			for i < len(chars) && !unicode.IsSpace(chars[i]) && chars[i] != '(' && chars[i] != ')' && chars[i] != '"' {
				i++
			}
			token.value = string(chars[start:i])
			switch strings.ToUpper(token.value) {
			case "AND", "OR", "NOT":
				token.kind = strings.ToUpper(token.value)
			}
		}
		if token.kind == "tag" {
			token.value = strings.TrimPrefix(strings.TrimSpace(token.value), "#")
			if token.value == "" {
				return nil, fmt.Errorf("теги: пустой тег, позиция %d", start+1)
			}
		}
		tokens = append(tokens, token)
		if len(tokens) > 256 {
			return nil, fmt.Errorf("выражение тегов слишком сложное (максимум 256 элементов)")
		}
	}
	return append(tokens, feedTagToken{kind: "end", position: len(chars) + 1}), nil
}
func (p *feedTagParser) current() feedTagToken { return p.tokens[p.at] }
func (p *feedTagParser) error(message string) error {
	return fmt.Errorf("теги: %s, позиция %d", message, p.current().position)
}
func (p *feedTagParser) or() (*feedTagNode, error) {
	left, err := p.and()
	for err == nil && p.current().kind == "OR" {
		p.at++
		var right *feedTagNode
		right, err = p.and()
		left = &feedTagNode{op: "OR", left: left, right: right}
	}
	return left, err
}
func (p *feedTagParser) and() (*feedTagNode, error) {
	left, err := p.unary()
	for err == nil {
		kind := p.current().kind
		if kind != "AND" && kind != "tag" && kind != "(" && kind != "NOT" {
			break
		}
		if kind == "AND" {
			p.at++
		}
		var right *feedTagNode
		right, err = p.unary()
		left = &feedTagNode{op: "AND", left: left, right: right}
	}
	return left, err
}
func (p *feedTagParser) unary() (*feedTagNode, error) {
	token := p.current()
	switch token.kind {
	case "NOT":
		p.at++
		child, err := p.unary()
		return &feedTagNode{op: "NOT", left: child}, err
	case "(":
		p.at++
		child, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.current().kind != ")" {
			return nil, p.error("ожидалась закрывающая скобка")
		}
		p.at++
		return child, nil
	case "tag":
		p.at++
		return &feedTagNode{op: "tag", tag: token.value}, nil
	default:
		return nil, p.error("ожидался тег, NOT или открывающая скобка")
	}
}
func feedTagBranches(node *feedTagNode, negate bool) ([]FeedTagClause, error) {
	if node.op == "NOT" {
		return feedTagBranches(node.left, !negate)
	}
	if node.op == "tag" {
		clause := FeedTagClause{Tags: []string{node.tag}}
		if negate {
			clause = FeedTagClause{Exclude: []string{node.tag}}
		}
		return []FeedTagClause{clause}, nil
	}
	left, err := feedTagBranches(node.left, negate)
	if err != nil {
		return nil, err
	}
	right, err := feedTagBranches(node.right, negate)
	if err != nil {
		return nil, err
	}
	isOr := node.op == "OR"
	if negate {
		isOr = !isOr
	}
	var clauses []FeedTagClause
	if isOr {
		clauses = append(left, right...)
	} else {
		for _, a := range left {
			for _, b := range right {
				clauses = append(clauses, FeedTagClause{Tags: append(append([]string{}, a.Tags...), b.Tags...), Exclude: append(append([]string{}, a.Exclude...), b.Exclude...)})
			}
		}
	}
	return uniqueFeedClauses(clauses)
}
func uniqueFeedClauses(clauses []FeedTagClause) ([]FeedTagClause, error) {
	out := []FeedTagClause{}
	seen := map[string]bool{}
	for _, clause := range clauses {
		clause.Tags = cleanFeedTerms(clause.Tags)
		clause.Exclude = cleanFeedTerms(clause.Exclude)
		excluded := map[string]bool{}
		for _, tag := range clause.Exclude {
			excluded[strings.ToLower(tag)] = true
		}
		impossible := false
		for _, tag := range clause.Tags {
			impossible = impossible || excluded[strings.ToLower(tag)]
		}
		if impossible {
			continue
		}
		raw, _ := json.Marshal(clause)
		if seen[string(raw)] {
			continue
		}
		seen[string(raw)] = true
		out = append(out, clause)
		if len(out) > maxFeedBranches {
			return nil, fmt.Errorf("выражение тегов слишком сложное: максимум %d ветвей OR после раскрытия скобок", maxFeedBranches)
		}
	}
	return out, nil
}

func feedQueryTerms(terms []string) []string {
	out := make([]string, len(terms))
	for i, term := range terms {
		out[i] = term
		if strings.ContainsAny(term, " \t\r\n") {
			raw, _ := json.Marshal(term)
			out[i] = string(raw)
		}
	}
	return out
}
