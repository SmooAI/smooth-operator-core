/**
 * Structured output — a guaranteed-JSON answer conforming to a caller-supplied
 * JSON Schema. Port of the Rust reference's `ResponseFormat`
 * (`rust/smooth-operator-core/src/llm.rs`, SMOODEV-1472).
 *
 * Wire mapping (OpenAI-compatible, e.g. the LiteLLM gateway at `llm.smoo.ai`):
 * serialized on `/chat/completions` as
 *
 * ```json
 * { "response_format": { "type": "json_schema",
 *                        "json_schema": { "name": …, "schema": …, "strict": … } } }
 * ```
 *
 * Usage follows this package's existing `metadataField` idiom — a spreadable
 * fragment, so an unset format contributes nothing and the wire stays
 * byte-identical:
 *
 * ```ts
 * const response = await client.chat.completions.create({
 *     model, messages, ...responseFormatField(jsonSchemaFormat('weather', schema)),
 * });
 * const report = structuredJson<WeatherReport>(response);
 * ```
 *
 * The Rust engine also has an Anthropic-native path (no `response_format` field
 * there, so it forces a synthetic single tool call whose `input_schema` IS the
 * requested schema). This engine talks to OpenAI-compatible endpoints only, so
 * there is nothing to mirror.
 */

/** Constrains the model's reply to a named JSON Schema. */
export interface ResponseFormat {
    /** A short identifier for the schema, e.g. `weather_report`. */
    name: string;
    /** The JSON Schema the response object must conform to. */
    schema: Record<string, unknown>;
    /** Request exact schema adherence (no extra keys). OpenAI/LiteLLM enforce it. */
    strict: boolean;
}

/** A strict JSON-schema response format — the analogue of Rust's `ResponseFormat::json_schema`. */
export function jsonSchemaFormat(name: string, schema: Record<string, unknown>): ResponseFormat {
    return { name, schema, strict: true };
}

/** The OpenAI-compatible `response_format` wire object. */
interface ResponseFormatWire {
    type: 'json_schema';
    json_schema: { name: string; schema: Record<string, unknown>; strict: boolean };
}

/**
 * Spreadable `{ response_format }` fragment for a model request body. An absent
 * format yields `{}`, so the wire stays byte-identical when unset (Rust parity:
 * the field is `Option` and skipped when `None`). Mirrors {@link metadataField}.
 */
export function responseFormatField(format?: ResponseFormat): { response_format?: ResponseFormatWire } {
    if (!format) return {};
    return {
        response_format: {
            type: 'json_schema',
            json_schema: { name: format.name, schema: format.schema, strict: format.strict },
        },
    };
}

/** The slice of a chat completion these helpers read. */
interface CompletionLike {
    choices: Array<{ message: { content?: string | null } }>;
}

/** First 200 characters (not bytes) — matches the Rust reference's truncation. */
function snippet(text: string): string {
    return [...text].slice(0, 200).join('');
}

/**
 * Parse a completion's content as JSON. For a structured-output response this is
 * the schema-conforming object the model produced.
 *
 * Throws if the content is empty or is not valid JSON, quoting a truncated
 * snippet of the offending content so a model that ignored the schema can be
 * diagnosed from the error alone. Never silently returns an empty value.
 *
 * The type parameter is an assertion, not a validation — TypeScript has no
 * runtime types, so unlike Rust's `deserialize_json` nothing here can check the
 * parsed value against `T`. There is deliberately no separate `deserializeJson`:
 * it would be this function with a cast, and pretending otherwise would imply a
 * guarantee this engine cannot make.
 */
export function structuredJson<T = unknown>(response: CompletionLike): T {
    const content = (response.choices[0]?.message?.content ?? '').trim();
    if (content === '') {
        throw new Error('structured output: model returned empty content (expected a JSON object)');
    }
    try {
        return JSON.parse(content) as T;
    } catch (error) {
        const reason = error instanceof Error ? error.message : String(error);
        throw new Error(`structured output: response content was not valid JSON (${reason}): ${snippet(content)}`);
    }
}
