import { describe, expect, it } from 'vitest';

import { jsonSchemaFormat, responseFormatField, structuredJson } from '../src/structured.js';

const schema = { type: 'object', properties: { city: { type: 'string' } } };

function completion(content: string | null) {
    return { choices: [{ message: { content } }] };
}

describe('jsonSchemaFormat', () => {
    it('is strict by default', () => {
        expect(jsonSchemaFormat('weather_report', schema)).toEqual({ name: 'weather_report', schema, strict: true });
    });
});

describe('responseFormatField', () => {
    it('renders the OpenAI-compatible wire object', () => {
        expect(responseFormatField(jsonSchemaFormat('weather', schema))).toEqual({
            response_format: { type: 'json_schema', json_schema: { name: 'weather', schema, strict: true } },
        });
    });

    // The parity bar: unset contributes nothing, so the request body is
    // byte-identical to one built before this feature existed.
    it('yields an empty fragment when unset', () => {
        expect(responseFormatField()).toEqual({});
        expect(JSON.stringify({ model: 'm', ...responseFormatField() })).toBe('{"model":"m"}');
    });
});

describe('structuredJson', () => {
    it('parses the completion content', () => {
        expect(structuredJson(completion('  {"city":"Indianapolis","high":82}  '))).toEqual({ city: 'Indianapolis', high: 82 });
    });

    it('rejects empty content', () => {
        expect(() => structuredJson(completion('   '))).toThrow(/empty content/);
        expect(() => structuredJson(completion(null))).toThrow(/empty content/);
    });

    it('rejects non-JSON and quotes the offending content', () => {
        expect(() => structuredJson(completion("I'm sorry, I can't do that."))).toThrow(/not valid JSON/);
        expect(() => structuredJson(completion("I'm sorry, I can't do that."))).toThrow(/I'm sorry/);
    });

    it('truncates the snippet at 200 characters', () => {
        try {
            structuredJson(completion('x'.repeat(500)));
            throw new Error('expected a throw');
        } catch (error) {
            expect((error as Error).message).toMatch(new RegExp(`: x{200}$`));
        }
    });
});
