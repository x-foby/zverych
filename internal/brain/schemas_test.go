package brain

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
)

func properties(s map[string]any) map[string]any {
	p, _ := s["properties"].(map[string]any)
	return p
}

func hasProperty(s *llm.Schema, name string) bool {
	if s == nil {
		return false
	}
	_, ok := properties(s.Schema)[name]
	return ok
}

// itemsOf is the schema of an array property's items.
func itemsOf(s map[string]any, name string) map[string]any {
	items, _ := properties(s)[name].(map[string]any)["items"].(map[string]any)
	return items
}

// jsonFields lists the json tags of a struct's fields.
func jsonFields(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := range t.NumField() {
		out = append(out, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return out
}

func propertyNames(s map[string]any) []string { return slices.Sorted(maps.Keys(properties(s))) }

func enum(s map[string]any, name string) []string {
	e, _ := properties(s)[name].(map[string]any)["enum"].([]string)
	return e
}

// strict checks what strict mode needs of every object: all properties required, no others.
func strict(t *testing.T, where string, v any) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if m["type"] == "object" {
		if !reflect.DeepEqual(m["required"], propertyNames(m)) || m["additionalProperties"] != false {
			t.Errorf("%s: not strict: required %v, properties %v, additionalProperties %v", where, m["required"],
				propertyNames(m), m["additionalProperties"])
		}
		for k, p := range properties(m) {
			strict(t, where+"."+k, p)
		}
	}
	if items, ok := m["items"]; ok {
		strict(t, where+"[]", items)
	}
}

func TestSchemasAreStrict(t *testing.T) {
	for _, s := range []*llm.Schema{evaluateSchema(false), evaluateSchema(true), commandSchema, privateCommandSchema} {
		strict(t, s.Name, s.Schema)
	}
}

// The schemas ask for what the parsers read, and nothing they do not.
func TestSchemasMatchParsers(t *testing.T) {
	eval := evaluateSchema(true).Schema
	if got := propertyNames(eval); !slices.Equal(got, []string{"actions", "rules", "threads"}) {
		t.Errorf("evaluation: %v", got)
	}
	if hasProperty(evaluateSchema(false), "threads") {
		t.Error("no threads in the prompt — none in the schema")
	}
	if got, want := propertyNames(itemsOf(eval, "rules")), slices.Sorted(slices.Values(jsonFields(ruleGroup{}))); !slices.Equal(got, want) {
		t.Errorf("rules: %v, ruleGroup: %v", got, want)
	}
	// An own action cites no instruction: "rule" and "text" come from the rule groups.
	if got := propertyNames(itemsOf(eval, "actions")); !slices.Equal(got, []string{"id", "react", "reason", "reply"}) {
		t.Errorf("actions: %v", got)
	}
	if got, want := propertyNames(itemsOf(eval, "threads")), slices.Sorted(slices.Values(jsonFields(assignment{}))); !slices.Equal(got, want) {
		t.Errorf("threads: %v, assignment: %v", got, want)
	}
	for _, field := range []string{"rules", "actions"} {
		if got := enum(itemsOf(eval, field), "react"); got[0] != "" || !slices.Equal(got[1:], allowedReactions) {
			t.Errorf("%s react: %v", field, got)
		}
	}

	fields := jsonFields(command{})
	both := map[string]bool{}
	for _, s := range []*llm.Schema{commandSchema, privateCommandSchema} {
		for _, p := range propertyNames(s.Schema) {
			if !slices.Contains(fields, p) {
				t.Errorf("%s: %q is not a field of command", s.Name, p)
			}
			both[p] = true
		}
		for _, c := range enum(s.Schema, "command") {
			if _, err := parseCommand(`{"command":"` + c + `"}`); err != nil {
				t.Errorf("%s: the parser rejects %q", s.Name, c)
			}
		}
	}
	for _, f := range fields {
		if !both[f] {
			t.Errorf("command field %q is in no schema", f)
		}
	}
	if !slices.Equal(enum(commandSchema.Schema, "command"), groupCommands) ||
		!slices.Equal(enum(privateCommandSchema.Schema, "command"), privateCommands) {
		t.Error("the command enums are the prompts' command lists")
	}
}

func TestChosenReaction(t *testing.T) {
	for in, want := range map[string]string{"": "", " ❤️ ": "❤", "🔥": "🔥", "none": "", "None": "", "нет": "", "-": "",
		"💩": "💩"} {
		if got := chosenReaction(in); got != want {
			t.Errorf("chosenReaction(%q) = %q, want %q", in, got, want)
		}
	}
}
