// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

// The request-envelope schemas of the MCP TypeScript SDK 1.32.1 (src/types.ts,
// vendored in lib/vendor/mcp-sdk/types.js, MIT), as predicates over parsed
// JSON (package jsonv, ParseLossless). A message the SDK accepted is accepted here and one
// it refused is refused: testdata/envelope-verdicts.json holds the SDK's own
// verdict for every probe and the tests hold these functions to it.
//
// zod semantics that matter: z.object accepts any non-null, non-array object
// and ignores unknown keys (strict ones refuse them); .optional() accepts an
// absent key but not null; z.number() refuses ±Infinity; .int() means a safe
// integer.

import (
	"math"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

const relatedTaskMetaKey = "io.modelcontextprotocol/related-task"

func asObject(v any) (*jsonv.Object, bool) {
	o, ok := v.(*jsonv.Object)
	return o, ok && o != nil
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func isBool(v any) bool {
	_, ok := v.(bool)
	return ok
}

func isFiniteNumber(v any) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// isAssertObject is the SDK's AssertObjectSchema: typeof "object" and not
// null, so arrays pass.
func isAssertObject(v any) bool {
	switch v.(type) {
	case *jsonv.Object, []any:
		return true
	}
	return false
}

// opt applies check to key when present.
func opt(o *jsonv.Object, key string, check func(any) bool) bool {
	v, ok := o.Get(key)
	return !ok || check(v)
}

func req(o *jsonv.Object, key string, check func(any) bool) bool {
	v, ok := o.Get(key)
	return ok && check(v)
}

// strictKeys reports whether o has no keys beyond allowed.
func strictKeys(o *jsonv.Object, allowed ...string) bool {
	for _, k := range o.Keys() {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func record(check func(any) bool) func(any) bool {
	return func(v any) bool {
		o, ok := asObject(v)
		if !ok {
			return false
		}
		for _, k := range o.Keys() {
			item, _ := o.Get(k)
			if !check(item) {
				return false
			}
		}
		return true
	}
}

func objectWith(checks func(o *jsonv.Object) bool) func(any) bool {
	return func(v any) bool {
		o, ok := asObject(v)
		return ok && checks(o)
	}
}

func literal(want string) func(any) bool {
	return func(v any) bool {
		s, ok := v.(string)
		return ok && s == want
	}
}

// IsRequestID is RequestIdSchema: a string or a safe integer.
func IsRequestID(v any) bool { return isString(v) || jsonv.IsSafeInteger(v) }

var isRequestMeta = objectWith(func(o *jsonv.Object) bool {
	return opt(o, "progressToken", IsRequestID) &&
		opt(o, relatedTaskMetaKey, objectWith(func(t *jsonv.Object) bool { return req(t, "taskId", isString) }))
})

func baseParams(o *jsonv.Object) bool { return opt(o, "_meta", isRequestMeta) }

var isBaseParams = objectWith(baseParams)

var isTaskMetadata = objectWith(func(o *jsonv.Object) bool { return opt(o, "ttl", isFiniteNumber) })

// IsTaskAugmentedParams is TaskAugmentedRequestParamsSchema.
func IsTaskAugmentedParams(v any) bool {
	o, ok := asObject(v)
	return ok && baseParams(o) && opt(o, "task", isTaskMetadata)
}

// IsJSONRPCRequest is JSONRPCRequestSchema (strict).
func IsJSONRPCRequest(v any) bool {
	o, ok := asObject(v)
	return ok && strictKeys(o, "jsonrpc", "id", "method", "params") &&
		req(o, "jsonrpc", literal(JSONRPCVersion)) && req(o, "id", IsRequestID) &&
		req(o, "method", isString) && opt(o, "params", isBaseParams)
}

func isJSONRPCNotification(v any) bool {
	o, ok := asObject(v)
	return ok && strictKeys(o, "jsonrpc", "method", "params") &&
		req(o, "jsonrpc", literal(JSONRPCVersion)) && req(o, "method", isString) && opt(o, "params", isBaseParams)
}

func isJSONRPCResultResponse(v any) bool {
	o, ok := asObject(v)
	return ok && strictKeys(o, "jsonrpc", "id", "result") &&
		req(o, "jsonrpc", literal(JSONRPCVersion)) && req(o, "id", IsRequestID) && req(o, "result", isBaseParams)
}

func isJSONRPCErrorResponse(v any) bool {
	o, ok := asObject(v)
	return ok && strictKeys(o, "jsonrpc", "id", "error") &&
		req(o, "jsonrpc", literal(JSONRPCVersion)) && opt(o, "id", IsRequestID) &&
		req(o, "error", objectWith(func(e *jsonv.Object) bool {
			return req(e, "code", jsonv.IsSafeInteger) && req(e, "message", isString)
		}))
}

// IsJSONRPCMessage is JSONRPCMessageSchema: a request, a notification, a
// result response or an error response.
func IsJSONRPCMessage(v any) bool {
	return IsJSONRPCRequest(v) || isJSONRPCNotification(v) || isJSONRPCResultResponse(v) || isJSONRPCErrorResponse(v)
}

var isIcon = objectWith(func(o *jsonv.Object) bool {
	return req(o, "src", isString) && opt(o, "mimeType", isString) &&
		opt(o, "sizes", arrayOf(isString)) &&
		opt(o, "theme", func(v any) bool { return literal("light")(v) || literal("dark")(v) })
})

func arrayOf(check func(any) bool) func(any) bool {
	return func(v any) bool {
		arr, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range arr {
			if !check(item) {
				return false
			}
		}
		return true
	}
}

var isImplementation = objectWith(func(o *jsonv.Object) bool {
	return req(o, "name", isString) && opt(o, "title", isString) && opt(o, "icons", arrayOf(isIcon)) &&
		req(o, "version", isString) && opt(o, "websiteUrl", isString) && opt(o, "description", isString)
})

// isElicitationCapability: the SDK preprocesses {} to {form: {}}, then
// intersects an object (form, url) with a string record, so any plain object
// whose form and url, when present, have the right shape.
var isElicitationCapability = objectWith(func(o *jsonv.Object) bool {
	return opt(o, "form", objectWith(func(f *jsonv.Object) bool { return opt(f, "applyDefaults", isBool) })) &&
		opt(o, "url", isAssertObject)
})

var isClientTasksCapability = objectWith(func(o *jsonv.Object) bool {
	return opt(o, "list", isAssertObject) && opt(o, "cancel", isAssertObject) &&
		opt(o, "requests", objectWith(func(r *jsonv.Object) bool {
			return opt(r, "sampling", objectWith(func(s *jsonv.Object) bool { return opt(s, "createMessage", isAssertObject) })) &&
				opt(r, "elicitation", objectWith(func(e *jsonv.Object) bool { return opt(e, "create", isAssertObject) }))
		}))
})

var isClientCapabilities = objectWith(func(o *jsonv.Object) bool {
	return opt(o, "experimental", record(isAssertObject)) &&
		opt(o, "sampling", objectWith(func(s *jsonv.Object) bool {
			return opt(s, "context", isAssertObject) && opt(s, "tools", isAssertObject)
		})) &&
		opt(o, "elicitation", isElicitationCapability) &&
		opt(o, "roots", objectWith(func(r *jsonv.Object) bool { return opt(r, "listChanged", isBool) })) &&
		opt(o, "tasks", isClientTasksCapability) &&
		opt(o, "extensions", record(isAssertObject))
})

// IsInitializeRequest is InitializeRequestSchema. Like every method schema
// it checks method and params only; jsonrpc and id are not its business.
func IsInitializeRequest(v any) bool {
	o, ok := asObject(v)
	return ok && req(o, "method", literal("initialize")) &&
		req(o, "params", objectWith(func(p *jsonv.Object) bool {
			return baseParams(p) && req(p, "protocolVersion", isString) &&
				req(p, "capabilities", isClientCapabilities) && req(p, "clientInfo", isImplementation)
		}))
}

// IsPingRequest is PingRequestSchema.
func IsPingRequest(v any) bool {
	o, ok := asObject(v)
	return ok && req(o, "method", literal("ping")) && opt(o, "params", isBaseParams)
}

// IsListToolsRequest is ListToolsRequestSchema.
func IsListToolsRequest(v any) bool {
	o, ok := asObject(v)
	return ok && req(o, "method", literal("tools/list")) &&
		opt(o, "params", objectWith(func(p *jsonv.Object) bool { return baseParams(p) && opt(p, "cursor", isString) }))
}

// IsCallToolRequest is CallToolRequestSchema.
func IsCallToolRequest(v any) bool {
	o, ok := asObject(v)
	return ok && req(o, "method", literal("tools/call")) &&
		req(o, "params", objectWith(func(p *jsonv.Object) bool {
			return baseParams(p) && opt(p, "task", isTaskMetadata) && req(p, "name", isString) &&
				opt(p, "arguments", record(func(any) bool { return true }))
		}))
}

// methodSchema returns the schema of one of the four methods Pages serves.
func methodSchema(method string) func(any) bool {
	switch method {
	case "initialize":
		return IsInitializeRequest
	case "ping":
		return IsPingRequest
	case "tools/list":
		return IsListToolsRequest
	case "tools/call":
		return IsCallToolRequest
	}
	return nil
}
