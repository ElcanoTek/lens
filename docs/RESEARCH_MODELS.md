# Research model selection

Lens uses search-grounded research for websites that could not be scraped and
for CTV apps. TypeSafe categories run on that research summary after standard
classification succeeds. Research requests include enabled custom questions so
the summary can gather relevant evidence rather than only the standard rubric.

`--research-model` selects the model for both website fallback and CTV research.
The dashboard offers `perplexity/sonar-pro` (the default) and `perplexity/sonar`
(the cheaper option). Both search automatically in the existing chat-completions
request path. Merely advertising `web_search_options` in the OpenRouter catalog
does not establish that another model will search with that same request. Other
families need explicitly configured search tools/plugins and validation first.

## Live comparison, 2026-09-18

Four sequential requests per model from the production host, using Lens's
research prompts: Reuters, Defector, a deliberately nonexistent `.invalid`
domain, and Pluto TV. Temperature 0.2, 1,500 output-token budget for both models.
This budget matches website research but is lower than CTV's normal 2,000-token
default. No custom questions were included in this baseline comparison.

| Model | Total billed cost, 4 requests | Total elapsed time | Completion |
|---|---:|---:|---|
| Sonar | $0.02254 | 31.74 s | 4/4 stopped normally |
| Sonar Pro | $0.06590 | 45.82 s | Pluto TV reached the token limit |

Sonar was **66% cheaper** and **31% faster** in this sample, including the API's
reported search charges. Both identified the real entities and returned
`INSUFFICIENT INFORMATION` for the nonexistent domain. Their summaries included
citations. These are smoke-test observations, not independent fact verification
or a quality benchmark. Sonar Pro produced longer summaries; longer is not
automatically more useful to the classifier.

Recommendation: try Sonar per run for routine research; retain Sonar Pro as the
default until a larger labeled sample covers obscure apps and custom rubrics.
The production default has not been changed.

## Catalog findings

At the time of comparison, token prices per million were $1/$1 for Sonar versus
$3/$15 for Sonar Pro. Both advertised search starting at $5 per 1,000 requests;
actual usage-reported costs are the basis of the comparison above, not token-only
estimates. The dashboard displays search pricing separately.

`sonar-pro-search` advertised $18 per 1,000 search requests plus $3/$15 token
pricing, so it is not an obvious cost-saving upgrade. Reasoning/deep-research
models add complexity and potentially longer output to a bounded summarization
task. Low token prices on other families do not include the cost of enabling
search. None of these alternatives was live-benchmarked here.

Sources: [OpenRouter model catalog](https://openrouter.ai/api/v1/models) and
[web-search integration documentation](https://openrouter.ai/docs/guides/features/plugins/web-search).
