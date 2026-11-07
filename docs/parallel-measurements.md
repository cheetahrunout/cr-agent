# Several people on one engine: measurements

Measured on 2026-11-07 on the owner's PC: RTX 5090 32 GB, Qwen3.8-27B-UD-Q5_K_XL, llama.cpp b12125 CUDA 13.4, `-c 131072 -ctk f16 -ctv f16 -fa on -ngl all -t 8`. The engine ran directly on 127.0.0.1, not through the agent or its sandbox. Answers had a fixed length (`ignore_eos`, 600 tokens) and short prompts (about 65 tokens), unless said otherwise. Script: a node script that started `llama-server` per configuration and sent requests at the same time.

## Speed

| Slots (`-np`) | KV | People asking at once | Speed each | Total |
|---|---|---|---|---|
| 1 | split | 1 | 67 tok/s | 67 tok/s |
| 2 | split | 2 | 62 tok/s | 124 tok/s |
| 4 | split | 4 | 56 tok/s | 221 tok/s |
| 4 | shared (`-kvu`) | 4 | 56 tok/s | 224 tok/s |

One person alone on a 2- or 4-slot engine gets the same 67 tok/s as on 1 slot.

## Memory

The total context sets the KV cache, so more slots with the same `-c` cost almost nothing: 27.2 GB loaded with 1 slot, 27.3 GB with 2, 27.6 GB with 4, and about 30.1 GB in use while 4 answered.

## Long prompts

- Someone joining with a 58,000-token prompt: it took 20 s to read. The person already reading an answer slowed to about 29 tok/s on average over that time and never froze (longest pause 1 s).
- Split KV: each slot gets `-c / -np` (65,536 with 2, 32,768 with 4). A request larger than its slot is refused at once with `exceed_context_size_error`, and nobody else is affected.
- Shared KV (`-kvu`): four requests of about 72,000 tokens each, more than the pool together, were all accepted, worked for 49 s, then all four failed with "Context size has been exceeded". One person's long prompt can end everyone's answer. The engine recovered and served normally afterwards.
- Not measured: large requests that fit the shared pool together (the two-slot tests were sized wrongly: a filler word was about 3 tokens).

## Conclusion

Split KV: predictable, and people cannot hurt each other. Each person's context is the total divided by the slots, so the total stays what the host chose and the memory does not change. The cost is context per person, and a reload to change the number of slots.
