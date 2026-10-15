# AtlasCloud: one upstream account, six models, one customer key

Status: release candidate approved by independent code review. Production
deployment is authorized and awaiting publication. Customer prices
remain an operator decision. Test prices are fixtures, not selling prices.

## Account and group

1. Use one OpenAI API-key account with base URL
   `https://api.atlascloud.ai/api/v1`. Keep the AtlasCloud key in its server-side
   credential field; never distribute it to customers.
2. Bind this account to one OpenAI group. Enable image/media generation.
3. Add the following exact model IDs to the account and group model allowlists.
   Do not map them to another provider's similarly named model.
4. Configure customer image prices or explicit image/per-request model pricing.
   Configure each video model's per-second resolution prices in the group form.
   Empty AtlasCloud video cells block creation, not fallback to Grok prices.
5. Issue customer API keys for this group. Customers reuse the same key and
   select a model in each request.

| Model | Kind | Customer endpoint |
| --- | --- | --- |
| openai/gpt-image-2.5-flare/text-to-image | Image | POST /v1/images/generations |
| xai/grok-imagine-image-2.0-developer/text-to-image | Image | POST /v1/images/generations |
| openai/gpt-image-2/text-to-image | Image | POST /v1/images/generations |
| google/gemini-omni-1.1-flash/text-to-video | Video | POST /v1/videos/generations |
| minimax/h3-developer/text-to-video | Video | POST /v1/videos/generations |
| xai/grok-imagine-video/text-to-video | Video | POST /v1/videos/generations |

## Image examples

The adapter currently supports a single image (`n=1`), no streaming, no image
editing, and URL output (`response_format=url`). Invalid or unsupported options
are rejected before an upstream request. Responses contain `data[].url`.

```json
{
  "model": "openai/gpt-image-2.5-flare/text-to-image",
  "prompt": "A lighthouse beside a quiet sea",
  "n": 1,
  "size": "1024x1280",
  "quality": "medium",
  "output_format": "png",
  "response_format": "url"
}
```

Grok images use `aspect_ratio` and `resolution` (`1k` or `2k`) instead of pixel
dimensions:

```json
{
  "model": "xai/grok-imagine-image-2.0-developer/text-to-image",
  "prompt": "A lighthouse beside a quiet sea",
  "aspect_ratio": "16:9",
  "resolution": "1k",
  "response_format": "url"
}
```

The existing GPT Image 2 adapter retains its previous documented preset sizes,
quality (`low`, `medium`, `high`, or `auto`), and `jpeg`/`png` output options.

## Video examples

```json
{
  "model": "minimax/h3-developer/text-to-video",
  "prompt": "Gentle waves beside a lighthouse",
  "duration": 8,
  "resolution": "768P",
  "ratio": "16:9",
  "prompt_expansion": false
}
```

| Video model | Duration seconds | Resolution options | Ratio field |
| --- | --- | --- | --- |
| Gemini Omni | 3-10 (default 10) | 360p, 720p, 1080p, 4k | aspect_ratio |
| MiniMax H3 | 4-15 (default 8) | 480P, 768P, 1440p-sr, 4k-sr | ratio or aspect_ratio |
| Grok Imagine | 1-15 (default 8) | 480p, 720p | aspect_ratio |

MiniMax resolution values are case-sensitive in requests; its customer pricing
keys are normalized to lowercase. Gemini additionally supports `thinking_level`
(`default`, `high`, `low`) and an integer `seed`.

Use `Authorization: Bearer <CUSTOMER_KEY>` on every gateway request. Creation
returns `id` and `request_id` such as `atlascloud:task-123`. Poll
`GET /v1/videos/atlascloud:task-123` with the same customer key and group.
Statuses are `processing`, `completed`, or `failed`. Completed responses include
`video.url` and `outputs`. This is a JSON task interface, not OpenAI's multipart
video-upload protocol. Direct content proxying is not provided.

Video ownership is isolated by user, key, group, upstream account and provider
namespace. Completion billing uses a stable task ID and a short recoverable
claim lease, with durable request-id deduplication across create and status.
Lost billing snapshots fail closed; retry status later rather than resubmitting
a potentially billable generation job. Immediate completed creates share the
same completion claim with subsequent polls. A missing settlement marker is
reconciled from the committed database receipt, including archived receipts,
after ownership validation; later price changes cannot invalidate paid output.
The three image-model URLs and
completed video outputs are returned only after synchronous settlement.

## Verification boundary

Automated tests use stub upstream responses. They do not establish real AtlasCloud
key permissions, account balance, paid generation success, or live deployment.
Those require a separately approved production smoke test after configuration.

Local verification on 2026-10-15: full backend unit suite passed (23,045 tests,
58 packages; 19 tests skipped). Full frontend suite passed (2,870 tests,
372 files), as did TypeScript checking and the Vite production build.
Test fixtures now avoid a Unix-shell dependency, wait for actual asynchronous
cache completion, use distinct cooldown timestamps, and isolate process-wide
heap measurements without weakening their assertions. The Linux amd64
embedded-frontend release build also passed. Independent release review found
no remaining concrete P1/P2 issues in the reviewed scope. Production deployment
remains pending at the time of writing. Official image/video docs were checked
read-only and both use `data.outputs[0]` for completed predictions.
