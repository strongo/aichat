package llmdecider

// decisionSchema is the WIRE JSON Schema for decision.Decision sent as
// ai.ChatRequest.ResponseSchema, written to satisfy OpenAI's strict
// structured-output mode (see ai.ChatRequest.StrictSchema): every property
// is listed in "required" (nullability is expressed with a ["T","null"]
// type array, not by omission), and every object -- including the root --
// sets "additionalProperties": false.
//
// It intentionally does NOT mirror decision.Decision's Go JSON shape
// field-for-field: strict mode has no notion of an open map, so "slots"
// (decision.Decision.Slots, a map[string]string) is instead an array of
// {name, value} objects on the wire and mapped back to the map after
// parsing -- see wireDecision.toDecision.
//
// Kept as a static, tested constant (rather than reflected from the struct)
// so a schema change is a visible, reviewed diff. Keep it in sync with
// ai/decision.Decision and wireDecision; TestDecisionSchema_MatchesFields
// and TestDecisionSchema_IsStrictValid in schema_test.go check both.
const decisionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "module": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "value": {"type": "string"},
        "confidence": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "required": ["value", "confidence"]
    },
    "intent": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "value": {"type": "string"},
        "confidence": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "required": ["value", "confidence"]
    },
    "interaction": {
      "type": "string",
      "enum": ["command", "question", "confirmation", "rejection", "correction", "continuation", "cancellation", "undo", "chat"]
    },
    "reference": {
      "type": ["object", "null"],
      "additionalProperties": false,
      "properties": {
        "kind": {"type": "string"},
        "expression": {"type": "string"},
        "pronoun": {"type": "boolean"}
      },
      "required": ["kind", "expression", "pronoun"]
    },
    "requiredScopes": {"type": "array", "items": {"type": "string"}},
    "requiredData": {"type": "array", "items": {"type": "string"}},
    "slots": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "name": {"type": "string"},
          "value": {"type": "string"}
        },
        "required": ["name", "value"]
      }
    },
    "canHandleDeterministically": {"type": "boolean"},
    "needsLLM": {"type": "boolean"},
    "presentation": {"type": ["string", "null"]}
  },
  "required": ["module", "intent", "interaction", "reference", "requiredScopes", "requiredData", "slots", "canHandleDeterministically", "needsLLM", "presentation"]
}`
