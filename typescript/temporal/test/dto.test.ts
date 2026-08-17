/**
 * Unit tests for the activity DTO boundary — the TS sibling of the Rust crate's
 * `dto.rs` tests. These run with **no Temporal runtime**: they prove the model
 * response projects to a JSON-serializable DTO and reconstructs the fields the
 * deterministic `driveTurn` loop reads, i.e. that it can actually cross an
 * activity boundary.
 */

import { describe, expect, it } from 'vitest';

import type { ModelResponse } from '@smooai/smooth-operator-core/executor';

import { modelResponseToOutput, outputToModelResponse, type ModelCallInput, type ToolInvokeInput } from '../src/dto.js';

function sampleResponse(): ModelResponse {
    return {
        choices: [
            {
                message: {
                    content: 'hello',
                    tool_calls: [{ id: 'call-1', function: { name: 'echo', arguments: '{"text":"hi"}' } }],
                },
            },
        ],
        usage: { prompt_tokens: 10, completion_tokens: 5 },
    } as ModelResponse;
}

describe('ModelCallOutput', () => {
    it('projects a ModelResponse then reconstructs the fields the loop reads', () => {
        const dto = modelResponseToOutput(sampleResponse());
        expect(dto.content).toBe('hello');
        expect(dto.toolCalls).toHaveLength(1);
        expect(dto.toolCalls[0].id).toBe('call-1');
        expect(dto.toolCalls[0].function.name).toBe('echo');
        expect(dto.usage).toEqual({ prompt_tokens: 10, completion_tokens: 5 });

        const restored = outputToModelResponse(dto);
        expect(restored.choices[0].message.content).toBe('hello');
        expect(restored.choices[0].message.tool_calls).toHaveLength(1);
        expect(restored.choices[0].message.tool_calls?.[0].function.arguments).toBe('{"text":"hi"}');
        expect(restored.usage).toEqual({ prompt_tokens: 10, completion_tokens: 5 });
    });

    it('survives a JSON serialize/deserialize round trip (crosses an activity boundary)', () => {
        const dto = modelResponseToOutput(sampleResponse());
        const back = JSON.parse(JSON.stringify(dto)) as typeof dto;
        expect(back.content).toBe('hello');
        expect(back.toolCalls[0].function.name).toBe('echo');
        expect(back.usage?.prompt_tokens).toBe(10);
    });

    it('a tool-only reply (no content) projects to empty content + null tool_calls when reconstructed empty', () => {
        const toolOnly = {
            choices: [{ message: { content: null, tool_calls: [{ id: 'c1', function: { name: 't', arguments: '{}' } }] } }],
            usage: null,
        } as ModelResponse;
        const dto = modelResponseToOutput(toolOnly);
        expect(dto.content).toBe('');
        expect(dto.toolCalls).toHaveLength(1);
        expect(dto.usage).toBeUndefined();

        // A response with no tool calls reconstructs to null tool_calls (OpenAI wire shape).
        const noTools = outputToModelResponse({ content: 'done', toolCalls: [] });
        expect(noTools.choices[0].message.tool_calls).toBeNull();
    });
});

describe('activity inputs', () => {
    it('serialize cleanly (JSON round trip)', () => {
        const modelInput: ModelCallInput = {
            messages: [{ role: 'user', content: 'hi' }],
            tools: [{ type: 'function', function: { name: 'echo', description: 'echo', parameters: { type: 'object' } } }],
        };
        const backModel = JSON.parse(JSON.stringify(modelInput)) as ModelCallInput;
        expect(backModel.messages).toHaveLength(1);
        expect((backModel.tools[0].function as { name: string }).name).toBe('echo');

        const toolInput: ToolInvokeInput = { call: { id: 'c1', name: 'echo', arguments: {} } };
        const backTool = JSON.parse(JSON.stringify(toolInput)) as ToolInvokeInput;
        expect(backTool.call.name).toBe('echo');
    });
});
