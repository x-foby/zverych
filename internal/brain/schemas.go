package brain

import (
	"maps"
	"slices"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// JSON schemas of the answers the code parses (structured outputs, SPEC 7): with a model that
// supports them the provider constrains the answer, so it is always one object of this shape.
// The prompts keep describing the format: a model without structured outputs follows the text,
// and the parsers stay tolerant. Strict mode — every field required, no others — is what
// providers support alike, so optional fields are required here and empty when unused.

// schemaObject is a strict schema object: all of props are required, nothing else is allowed.
func schemaObject(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": slices.Sorted(maps.Keys(props)),
		"additionalProperties": false}
}

func schemaArray(item map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": item}
}

func schemaEnum(values []string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

var (
	schemaInt    = map[string]any{"type": "integer"}
	schemaBool   = map[string]any{"type": "boolean"}
	schemaString = map[string]any{"type": "string"}
	// schemaReaction is one of the reactions the bot may set, or "" — none.
	schemaReaction = schemaEnum(append([]string{""}, allowedReactions...))
)

// evaluateSchema is the answer of evaluatePrompt; "threads" only when the prompt shows threads.
func evaluateSchema(withThreads bool) *llm.Schema {
	props := map[string]any{
		"rules": schemaArray(schemaObject(map[string]any{
			"rule": schemaInt, "ids": schemaArray(schemaInt), "react": schemaReaction, "reply": schemaBool, "text": schemaString,
		})),
		"actions": schemaArray(schemaObject(map[string]any{
			"id": schemaInt, "react": schemaReaction, "reply": schemaBool, "reason": schemaString,
		})),
	}
	if withThreads {
		props["threads"] = schemaArray(schemaObject(map[string]any{"id": schemaInt, "thread": schemaInt}))
	}
	return &llm.Schema{Name: "evaluation", Schema: schemaObject(props)}
}

// modes are the values of "mode": a group's mode, or "" when the command has none.
var modes = []string{"", storage.ModeSilent, storage.ModeReactions, storage.ModeTalk}

// commandSchema is the answer of commandPrompt.
var commandSchema = &llm.Schema{Name: "command", Schema: schemaObject(map[string]any{
	"no_reply": schemaBool, "command": schemaEnum(groupCommands), "person": schemaInt, "self": schemaBool, "scope": schemaEnum([]string{"chat", "topic"}),
	"topic": schemaInt, "text": schemaString, "ids": schemaArray(schemaInt), "mode": schemaEnum(modes),
})}

// privateCommandSchema is the answer of privateCommandPrompt.
var privateCommandSchema = &llm.Schema{Name: "private_command", Schema: schemaObject(map[string]any{
	"no_reply": schemaBool, "command": schemaEnum(privateCommands), "chat": schemaInt, "person": schemaInt, "people": schemaArray(schemaInt),
	"names": schemaArray(schemaString), "self": schemaBool, "topic": schemaInt, "text": schemaString, "ids": schemaArray(schemaInt), "mode": schemaEnum(modes),
})}
