---
'@smooai/smooth-operator-core': minor
---

th-25ce5c: Multimodal message content — carry image attachments through the conversation model and emit them as OpenAI `image_url` content parts.

`Message` gains an `images: Vec<ImageContent>` field (a new `Message::user_with_images` constructor) that the OpenAI-compat LLM client serializes as a standard multimodal content-parts array (`[{type:text,...},{type:image_url,image_url:{url,detail}}]`) when a user message carries images. Text-only turns are byte-identical to before (`skip_serializing_if` omits the field), so no regression on non-vision chat. The prompt-cache marker path is guarded to pass image parts through untouched rather than flattening them into a text block (which would silently drop the images). Foundation for Big Smooth's vision/document support (epic th-3be564); consumed downstream by a git-rev bump.
