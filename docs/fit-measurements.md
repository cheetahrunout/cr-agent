# The pre-load check against the real engine

Measured on the RTX 5090 (32,607 MiB, driver 610.88) with llama.cpp b12125, at low integrity, reading the models in place. "Measured" is the rise in `nvidia-smi` memory used, 1.5 s after the engine reported healthy. "Estimate" is what the check holds against free memory: `fit.For` plus the 512 MiB reserve. To repeat it:

    CRAGENT_TEST_FIT=<model folder> CRAGENT_ENGINE_DIR=<engine> go test -v -run TestFitAgainstTheRealEngine ./internal/supervisor

| Model | Layers | Context | Weights + KV + compute + reserve (MiB) | Estimate (MiB) | Measured (MiB) | Ratio |
|---|---|---|---|---|---|---|
| Qwen3.8-27B-UD-Q5_K_XL (`qwen35`) | all | 32768 | 18437 + 2048 + 741 + 512 | 21738 | 20993 | 1.035 |
| Qwen3.8-27B-UD-Q5_K_XL (`qwen35`) | all | 8192 | 18437 + 512 + 741 + 512 | 20202 | 19428 | 1.040 |
| Qwen3.6Q4_K_XL (`qwen35`) | all | 32768 | 16386 + 2048 + 741 + 512 | 19687 | 19015 | 1.035 |
| Qwen3.6Q6_K (`qwen35moe`) | 30 | 16384 | 20070 + 256 + 741 + 512 | 21579 | 21183 | 1.019 |
| Qwen3.6Q6_K (`qwen35moe`) | 20 | 8192 | 13393 + 80 + 741 + 512 | 14726 | 14292 | 1.030 |

Before this change the first row estimated 28010 MiB, a ratio of 1.33, and refused a model that fits.

## Why it was too high

These models did not reach the unknown-layout fallback. Their header states `head_count_kv` 4 and `key_length` 256, and the check gave every one of the 65 blocks a KV cache of that size. They are hybrid models: in llama.cpp only every 4th block has attention and a KV cache (`full_attention_interval`, default 4, in `src/models/qwen35.cpp`), and the rest use linear attention, with no cache that grows with the context. That was 6272 MiB too much at 32768.

## The layout rules now

All from the pinned source, llama.cpp commit 7d516e25e:

- Per block: `n_head_kv(il) × (key_length + value_length) × bytes per value × tokens`, for any architecture, as `llama_kv_cache` sizes it. A missing `key_length` or `head_count_kv` takes llama.cpp's own default.
- A block holds no cache if the file's `attention.recurrent_layers` or `full_attention_interval` says so, if its `head_count_kv` is 0, or by the default interval of `qwen35`, `qwen35moe`, `qwen3next`, `qwen4exp` (4) and `minimax-01` (8).
- No cache that grows with the context: what `create_memory` gives no memory (the `bert` family, `wavtokenizer-dec`, `gemma-embedding`, `dream`, `llada`, `rnd1`, `clef`) and `llm_arch_is_recurrent` (`mamba`, `rwkv`).
- Sliding-window blocks hold `n_swa + n_ubatch` cells, padded, by the file's pattern or the default of `load_swa_pattern`. If neither says which blocks, every block counts as full: a little over, never far under.
- The 20 % fallback is only for a header with neither head counts nor a way to derive them.

Only the two hybrid architectures above were measured. The other rules come from reading the source.

## What the check does with an estimate

A confident estimate that does not fit is refused. An estimate on the fallback does not refuse: the agent logs it and lets the engine try.

A real out-of-memory load ends as an engine error with `insufficient_vram`, the code of the pre-load check, in 6.7 s (Qwen3.6Q6_K with the job's memory capped at 2 GiB: exit code 1, `cudaMalloc failed: out of memory`, and the GPU back at rest).

**A full GPU does not cause that on this driver.** With another engine holding 21 GB, Qwen3.6Q6_K (28 GB) still loaded, in 41 s against about 10 s, far past the card's 32,607 MiB: the driver spills to system memory. So a load the check lets through can succeed and run slowly, not fail. The job's memory cap (RAM minus 4 GiB) is then what bounds it. Speed in that state was not measured. On other drivers or settings this may fail instead.
