package controllers

// miniship (miniship-cloud#1305, public ADR 0077): what one item of a read's
// `select` may be, and a column named in a filter or an order. The Supabase
// SDK's postgrest-js writes four shapes of item:
//
//	id                       a column
//	alias:column             the column, answered under another key
//	column::text             the column cast to a type
//	column->a->>b            a path into a json column, `->` giving json and
//	                         `->>` text
//	other(*)                 related rows of another table (postgrest_embed.go)
//
// A column in a filter (`data->>a=eq.x`) and in an order (`order=data->a.desc`)
// takes the same path. Nothing the caller sent becomes SQL text but a name that
// passed pgrIdent and was quoted, a key of a path that passed pgrPathKey and
// was quoted as a literal, a number, or a type that passed pgrCastType.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// pgrPathKey is a key of a json path: letters, digits and _, as an object's
// key or an array's index.
var pgrPathKey = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)

// pgrCastType is a type's name as a cast writes it: `text`, `int4`,
// `numeric(10,2)`, `double precision`, `public.mood`, `text[]`. It is a run of
// plain words, so it can name a type and cannot be an expression.
var pgrCastType = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?( [A-Za-z_][A-Za-z0-9_]*)*(\([0-9]{1,4}(,[0-9]{1,4})?\))?(\[\])?$`)

// pgrStep is one `->key` or `->>key` of a json path.
type pgrStep struct {
	key  string
	text bool
}

// pgrColumn is a column and the json path into it, if any.
type pgrColumn struct {
	name  string
	steps []pgrStep
}

// pgrParseColumn reads `name`, `name->a`, `name->a->>b` and `name->0`.
func pgrParseColumn(raw string) (pgrColumn, error) {
	name, rest, _ := strings.Cut(raw, "->")
	column := pgrColumn{name: strings.TrimSpace(name)}
	if err := pgrCheckName(column.name, "column"); err != nil {
		return pgrColumn{}, err
	}
	if !strings.Contains(raw, "->") {
		return column, nil
	}
	rest = "->" + rest
	for rest != "" {
		text := false
		switch {
		case strings.HasPrefix(rest, "->>"):
			text, rest = true, rest[3:]
		case strings.HasPrefix(rest, "->"):
			rest = rest[2:]
		}
		key := rest
		if i := strings.Index(rest, "->"); i >= 0 {
			key, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		if !pgrPathKey.MatchString(key) {
			return pgrColumn{}, pgrRefuse("a key of a json path is letters, digits and _")
		}
		if text && rest != "" {
			return pgrColumn{}, pgrRefuse("->> gives text, so nothing can follow it in a json path")
		}
		column.steps = append(column.steps, pgrStep{key: key, text: text})
	}
	return column, nil
}

// sql is the column, qualified by alias, with its path.
func (c pgrColumn) sql(alias string) string {
	text := pgrQuote(alias) + "." + pgrQuote(c.name)
	for _, step := range c.steps {
		operator := "->"
		if step.text {
			operator = "->>"
		}
		if _, err := strconv.Atoi(step.key); err == nil {
			text += operator + step.key
			continue
		}
		text += operator + pq.QuoteLiteral(step.key)
	}
	if len(c.steps) > 0 {
		text = "(" + text + ")"
	}
	return text
}

// label is the key the column is answered under unless an alias says
// otherwise: the last key of the path, or the column.
func (c pgrColumn) label() string {
	if len(c.steps) > 0 {
		return c.steps[len(c.steps)-1].key
	}
	return c.name
}

// pgrField is one column of a select, or `*`.
type pgrField struct {
	star  bool
	col   pgrColumn
	alias string
	cast  string
}

// pgrItem is one item of a select: a field or an embedded table.
type pgrItem struct {
	field *pgrField
	embed *pgrEmbed
}

// pgrSplitAlias cuts `alias:rest` at the first single colon before any
// parenthesis. `::` is a cast and is not cut.
func pgrSplitAlias(item string) (alias, rest string) {
	for i := 0; i < len(item); i++ {
		switch item[i] {
		case '(':
			return "", item
		case ':':
			if i+1 < len(item) && item[i+1] == ':' {
				i++
				continue
			}
			return item[:i], item[i+1:]
		}
	}
	return "", item
}

// pgrParseSelectList reads a select's items: `*`, columns with an alias, a
// cast or a json path, and embedded tables. depth counts the tables embedded
// above this list.
func pgrParseSelectList(raw string, depth int) ([]pgrItem, error) {
	var items []pgrItem
	for _, text := range pgrSplit(raw) {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, pgrRefuse("a select has an empty item")
		}
		if strings.HasPrefix(text, "...") {
			return nil, pgrRefuse("a spread of an embedded table is not answered here yet")
		}
		alias, rest := pgrSplitAlias(text)
		if alias != "" {
			if err := pgrCheckName(alias, "alias"); err != nil {
				return nil, err
			}
		}
		if open, cast := strings.Index(rest, "("), strings.Index(rest, "::"); open >= 0 && (cast < 0 || cast > open) {
			embed, err := pgrParseEmbed(alias, rest, open, depth)
			if err != nil {
				return nil, err
			}
			items = append(items, pgrItem{embed: embed})
			continue
		}
		field, err := pgrParseField(alias, rest)
		if err != nil {
			return nil, err
		}
		items = append(items, pgrItem{field: field})
	}
	return items, nil
}

// pgrParseField reads `*`, `column`, `column::type` and `column->a->>b`.
func pgrParseField(alias, raw string) (*pgrField, error) {
	if raw == "*" {
		if alias != "" {
			return nil, pgrRefuse("* cannot be given an alias")
		}
		return &pgrField{star: true}, nil
	}
	body, cast, hasCast := strings.Cut(raw, "::")
	field := &pgrField{alias: alias}
	if hasCast {
		cast = strings.TrimSpace(cast)
		if !pgrCastType.MatchString(cast) {
			return nil, pgrRefuse("a cast names a type: column::text")
		}
		field.cast = cast
	}
	column, err := pgrParseColumn(strings.TrimSpace(body))
	if err != nil {
		return nil, err
	}
	field.col = column
	return field, nil
}

// sql is the field, qualified by alias, as an item of a select list.
func (f *pgrField) sql(alias string) string {
	if f.star {
		return pgrQuote(alias) + ".*"
	}
	text := f.col.sql(alias)
	if f.cast != "" {
		text = fmt.Sprintf("CAST(%s AS %s)", text, f.cast)
	}
	label := f.alias
	if label == "" && (f.cast != "" || len(f.col.steps) > 0) {
		label = f.col.label()
		if f.cast != "" && len(f.col.steps) == 0 {
			label = f.col.name
		}
	}
	if label == "" || (label == f.col.name && f.cast == "" && len(f.col.steps) == 0) {
		return text
	}
	return text + " AS " + pgrQuote(label)
}
