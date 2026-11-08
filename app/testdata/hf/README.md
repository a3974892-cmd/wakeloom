Hand-built Hugging Face API responses (`/api/models/<repo>`, `/api/models/<repo>/tree/main?recursive=1`,
`model_index.json`) in the documented format, for custom_models_test.go. Field names and shapes follow
the public HF Hub API; file sizes are approximate.

`kimi/` mirrors a 2.78T-parameter transformers chat model (moonshotai/Kimi-K3.1-Pro, ~1.56 TB of shards) and the
answer of `/api/models?search=Kimi-K3.1-Pro&filter=gguf`; `kimi-gguf/` is the tree of the first GGUF hit.
