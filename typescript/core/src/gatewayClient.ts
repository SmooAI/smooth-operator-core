/**
 * A REAL OpenAI-compatible chat client for the agent's {@link ChatClientLike} seam.
 *
 * Until now the TypeScript engine could only be driven by {@link MockLlmProvider}
 * or a client the consumer hand-rolled; this is the shipped one, pointed at any
 * `/chat/completions` endpoint (the SmooAI gateway, or anything OpenAI-compatible)
 * with a base URL + API key.
 *
 * It is a thin adapter over the `openai` SDK, which this package already depends
 * on — the SDK owns the wire format, the SSE framing, timeouts and connection
 * retries, so the only thing this file adds is the two bits the SDK's parsed
 * response throws away:
 *
 * 1. **The gateway cost header.** Cost is reported ONLY in a response header, and
 *    on the streaming path those headers are gone once the SSE body is being
 *    consumed — the regression core#102 fixed in Rust. `.withResponse()` hands
 *    back the `Response` alongside the data, so both paths read the headers BEFORE
 *    touching the body and surface the parsed cost on the documented seam
 *    ({@link responseGatewayCost}): `headers` on a non-streaming response,
 *    `gatewayCostUsd` on a leading {@link ChatChunk} when streaming.
 * 2. **Nothing else.** The request body is passed through verbatim, so the
 *    agent's `metadata` (core#100) and every other field reach the wire unchanged.
 *
 * {@link MockLlmProvider} remains the default/test seam. This is opt-in: nothing
 * constructs it for you, so an unwired consumer behaves exactly as before.
 *
 * ```ts
 * const agent = new SmoothAgent(createGatewayClient({ baseURL: 'https://llm.smoo.ai/v1', apiKey: key }), options);
 * ```
 */
import OpenAI from 'openai';
import type { ChatCompletionCreateParamsNonStreaming, ChatCompletionCreateParamsStreaming } from 'openai/resources/chat/completions';

import type { ChatChunk, ChatClientLike } from './agent.js';
import { parseGatewayCost } from './cost.js';

/** The non-streaming response shape the {@link ChatClientLike} seam expects back. */
type ChatResponseLike = Awaited<ReturnType<ChatClientLike['chat']['completions']['create']>>;

export interface GatewayClientOptions {
    /** The OpenAI-compatible base URL, e.g. `https://llm.smoo.ai/v1`. */
    baseURL: string;
    /** Bearer key sent as `Authorization: Bearer <apiKey>`. */
    apiKey: string;
    /** Request timeout in milliseconds. Defaults to the SDK's own default. */
    timeoutMs?: number;
    /** Connection-level retries the SDK performs. Defaults to the SDK's own default. */
    maxRetries?: number;
}

/** Build a gateway-backed {@link ChatClientLike} from a base URL + key. */
export function createGatewayClient(options: GatewayClientOptions): ChatClientLike {
    return gatewayClientFrom(
        new OpenAI({
            baseURL: options.baseURL,
            apiKey: options.apiKey,
            ...(options.timeoutMs !== undefined ? { timeout: options.timeoutMs } : {}),
            ...(options.maxRetries !== undefined ? { maxRetries: options.maxRetries } : {}),
        }),
    );
}

/**
 * Wrap an already-configured `openai` SDK client — for consumers that need to own
 * the SDK's construction (custom `fetch`, proxy agent, default headers) and just
 * want the cost-header plumbing on top.
 */
export function gatewayClientFrom(sdk: OpenAI): ChatClientLike {
    return {
        chat: {
            completions: {
                async create(body: Record<string, unknown>) {
                    // `.withResponse()` keeps the raw Response next to the parsed body, which
                    // is the only way to see the cost header at all — the SDK's parsed
                    // response drops it. `headers` is the seam `responseGatewayCost` reads.
                    const { data, response } = await sdk.chat.completions
                        .create(body as unknown as ChatCompletionCreateParamsNonStreaming)
                        .withResponse();
                    return { ...data, headers: response.headers } as unknown as ChatResponseLike;
                },
                createStream(body: Record<string, unknown>): AsyncIterable<ChatChunk> {
                    return streamCompletion(sdk, body);
                },
            },
        },
    };
}

/**
 * Open a streaming completion and yield its chunks, with the gateway cost riding a
 * leading chunk.
 *
 * The ordering here is the load-bearing part: `.withResponse()` resolves as soon as
 * the response HEAD arrives, so the cost header is parsed while the SSE body is
 * still untouched. Reading it after iterating the stream would find nothing — that
 * is the exact bug core#102 fixed in Rust and core#121 called out for Go.
 *
 * The cost is emitted on its own leading chunk (matching Go's `ChatChunk{CostUSD}`)
 * so it is folded into the turn even if the stream errors before any usage arrives.
 * That chunk carries no `choices`, which the agent's stream loop skips.
 */
async function* streamCompletion(sdk: OpenAI, body: Record<string, unknown>): AsyncIterable<ChatChunk> {
    const { data, response } = await sdk.chat.completions
        .create({ ...body, stream: true } as unknown as ChatCompletionCreateParamsStreaming)
        .withResponse();
    const gatewayCostUsd = parseGatewayCost(response.headers);
    if (gatewayCostUsd !== undefined) {
        yield { choices: [], gatewayCostUsd };
    }
    for await (const chunk of data as unknown as AsyncIterable<ChatChunk>) {
        yield chunk;
    }
}
