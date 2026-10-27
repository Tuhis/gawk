### Builds and CPU per 10 ms frame

Emscripten: `emcc (Emscripten gcc/clang-like replacement + linker emulating GNU ld) 4.0.10 (b7dc6e5747465580df5984e723b9d1f10d8e804b)`. Node v24.21.0 (V8 13.6.233.17-node.53) on Intel Xeon E3-12xx v2 (Ivy Bridge, IBRS). Browser numbers are main-thread, best of 3 × 3000 frames; the worklet column is 1000 frames timed with `Date.now()` inside the AudioWorklet (1 ms clock, so ±0.001 ms/frame).

| Variant | wasm bytes | Node | chromium 156.0.8078.4 | firefox 157.0 | webkit 27.2 | chromium worklet | firefox worklet | webkit worklet |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `v02-avx` | 1,470,665 | 0.286 | 0.287 | 0.173 | 0.336 | 0.295 | 0.208 | 0.349 |
| `v02-avx2` | 1,471,317 | 0.289 | 0.293 | 0.186 | 0.361 | – | – | – |
| `mumble-avx` | 3,615,580 | 0.622 | – | – | – | – | – | – |
| `main-scalar` | 3,594,493 | 1.489 | 1.571 | 1.608 | 2.068 | – | – | – |
| `main-avx` | 3,615,580 | 0.582 | 0.615 | 0.289 | 0.664 | 0.586 | 0.314 | 0.669 |
| `main-avx2` | 3,616,232 | 0.591 | 0.675 | 0.301 | 0.699 | – | – | – |
| `main-little-scalar` | 1,603,835 | 0.686 | 0.844 | 0.761 | 0.952 | – | – | – |
| `main-little-avx` | 1,624,922 | 0.293 | 0.320 | 0.184 | 0.351 | 0.339 | 0.207 | 0.374 |
| `main-little-avx2` | 1,625,574 | 0.301 | 0.309 | 0.200 | 0.366 | – | – | – |

### Algorithmic delay and parity with native

| Variant | Output lag (samples) | Output lag (ms) | Probability lag (frames, best agreement with labels) |
| --- | --- | --- | --- |
| `v02-avx` | 960 | 20.00 | 1 |
| `mumble-avx` | 960 | 20.00 | 1 |
| `main-avx` | 960 | 20.00 | 2 |
| `main-little-avx` | 960 | 20.00 | 2 |

Against a native x86-64 build of the same main sources (`native-ref.c`, gcc -O2, SSE2 path):

| wasm variant | Output SNR vs native (dB) | Max |Δ probability| |
| --- | --- | --- |
| `main-scalar` | 63.3 | 0.0194 |
| `main-avx` | 63.2 | 0.0197 |
| `main-avx2` | 62.3 | 0.0184 |

### Denoising quality (offline, mean of three readings)

SI-SDR of the output against the clean reading (higher is better), input in brackets. Speech at −26 dBFS active level.

| Noise | SNR | `v02-avx` | `mumble-avx` | `main-avx` | `main-little-avx` |
| --- | --- | --- | --- | --- | --- |
| keyboard | 20 dB | 13.2 (19.1) | 13.2 (19.1) | 12.7 (19.1) | 11.2 (19.1) |
| keyboard | 10 dB | 10.9 (9.1) | 10.9 (9.1) | 10.8 (9.1) | 10.5 (9.1) |
| keyboard | 5 dB | 8.9 (4.1) | 9.0 (4.1) | 8.9 (4.1) | 8.7 (4.1) |
| keyboard | 0 dB | 6.7 (-0.9) | 6.9 (-0.9) | 6.7 (-0.9) | 6.5 (-0.9) |
| restaurant | 20 dB | 12.7 (19.1) | 12.7 (19.1) | 11.8 (19.1) | 10.9 (19.1) |
| restaurant | 10 dB | 9.5 (9.1) | 9.6 (9.1) | 9.0 (9.1) | 8.9 (9.1) |
| restaurant | 5 dB | 6.3 (4.1) | 6.6 (4.1) | 6.1 (4.1) | 6.1 (4.1) |
| restaurant | 0 dB | 1.8 (-0.9) | 2.4 (-0.9) | 2.3 (-0.9) | 2.3 (-0.9) |
| street | 20 dB | 13.5 (19.1) | 13.4 (19.1) | 12.7 (19.1) | 11.3 (19.1) |
| street | 10 dB | 11.9 (9.1) | 11.8 (9.1) | 11.3 (9.1) | 11.3 (9.1) |
| street | 5 dB | 10.5 (4.1) | 10.4 (4.1) | 9.9 (4.1) | 10.1 (4.1) |
| street | 0 dB | 8.6 (-0.9) | 8.6 (-0.9) | 8.3 (-0.9) | 8.4 (-0.9) |
| white | 20 dB | 13.4 (19.1) | 13.4 (19.1) | 12.1 (19.1) | 13.0 (19.1) |
| white | 10 dB | 12.3 (9.1) | 12.2 (9.1) | 12.0 (9.1) | 12.0 (9.1) |
| white | 5 dB | 11.2 (4.1) | 11.2 (4.1) | 11.1 (4.1) | 11.0 (4.1) |
| white | 0 dB | 9.8 (-0.9) | 9.8 (-0.9) | 9.7 (-0.9) | 9.7 (-0.9) |

Noise alone, 60 s: attenuation in dB at −36 dBFS (its level at 10 dB SNR).

| Noise | `v02-avx` | `mumble-avx` | `main-avx` | `main-little-avx` |
| --- | --- | --- | --- | --- |
| keyboard | 32.9 | 38.2 | 35.2 | 38.7 |
| restaurant | 7.9 | 9.3 | 28.9 | 28.9 |
| street | 39.2 | 49.7 | 46.8 | 44.6 |
| white | 54.5 | 50.2 | 40.3 | 32.2 |

### The talk gate (`v02-avx`)

In the output's time base. *Speech sent*: labelled speech frames the gate passed. *Clipped*: speech onsets (after ≥ 300 ms quiet) that lost ≥ 50 ms before the gate opened, pooled (clipped ÷ onsets over every run in the cell); *clip p90*: the 90th-percentile loss, ms. *Noise only*: 60 s of the noise with nobody talking, the share of frames sent and the spurts per minute. `energy-raw` is today's gate (RMS ≥ 0.01, 300 ms hold) on the unprocessed signal — pessimistic, because in production it runs after the browser's own suppression (§ Chromium below shows how little that removes from a keyboard); `energy-rnn` is the same gate after RNNoise; `prob-ON-OFF-HOLD` opens at probability ≥ ON, stays open while ≥ OFF, then holds HOLD ms; `probfloor-ON-OFF-HOLD-FLOOR` also needs the denoised block to be louder than FLOOR dBFS.

**Per noise, speech mixed at 10 and 5 dB SNR** (mean of three readings × two SNRs): speech sent · clipped.

| Gate | keyboard | restaurant | street | white |
| --- | --- | --- | --- | --- |
| `energy-raw` | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| `energy-rnn` | 99.2 % · 4.7 % | 99.3 % · 7.8 % | 99.3 % · 1.6 % | 99.3 % · 0.0 % |
| `prob-0.6-0.40-300` | 99.7 % · 1.6 % | 99.7 % · 0.0 % | 99.8 % · 0.0 % | 99.7 % · 0.0 % |
| `prob-0.7-0.50-300` | 99.7 % · 3.1 % | 99.7 % · 0.0 % | 99.8 % · 0.0 % | 99.7 % · 0.0 % |
| `prob-0.8-0.60-300` | 99.6 % · 4.7 % | 99.7 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `prob-0.9-0.70-300` | 99.5 % · 4.7 % | 99.6 % · 0.0 % | 99.6 % · 0.0 % | 99.5 % · 0.0 % |
| `probfloor-0.6-0.40-300--55` | 99.6 % · 4.7 % | 99.7 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `probfloor-0.6-0.40-300--50` | 99.6 % · 4.7 % | 99.6 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `probfloor-0.7-0.50-300--55` | 99.6 % · 4.7 % | 99.7 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `probfloor-0.7-0.50-300--50` | 99.6 % · 4.7 % | 99.6 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `probfloor-0.8-0.60-300--55` | 99.6 % · 4.7 % | 99.6 % · 0.0 % | 99.7 % · 0.0 % | 99.6 % · 0.0 % |
| `probfloor-0.8-0.60-300--50` | 99.5 % · 4.7 % | 99.6 % · 0.0 % | 99.6 % · 0.0 % | 99.5 % · 0.0 % |
| `probfloor-0.9-0.70-300--55` | 99.5 % · 4.7 % | 99.6 % · 0.0 % | 99.6 % · 0.0 % | 99.5 % · 0.0 % |
| `probfloor-0.9-0.70-300--50` | 99.5 % · 4.7 % | 99.6 % · 0.0 % | 99.5 % · 0.0 % | 99.4 % · 0.0 % |

**Noise only at -36 dBFS**, nobody talking: sent · spurts/min.

| Gate | keyboard | restaurant | street | white |
| --- | --- | --- | --- | --- |
| `energy-raw` | 96.2 % · 1.0 | 98.3 % · 5.0 | 90.3 % · 26.0 | 100.0 % · 1.0 |
| `energy-rnn` | 0.0 % · 0.0 | 53.1 % · 54.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `prob-0.6-0.40-300` | 4.3 % · 8.0 | 98.8 % · 5.0 | 2.1 % · 4.0 | 0.5 % · 1.0 |
| `prob-0.7-0.50-300` | 4.3 % · 8.0 | 98.7 % · 6.0 | 1.1 % · 2.0 | 0.5 % · 1.0 |
| `prob-0.8-0.60-300` | 2.7 % · 5.0 | 97.9 % · 9.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `prob-0.9-0.70-300` | 1.6 % · 3.0 | 97.3 % · 11.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.6-0.40-300--55` | 2.6 % · 5.0 | 96.2 % · 15.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `probfloor-0.6-0.40-300--50` | 1.5 % · 3.0 | 93.5 % · 16.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `probfloor-0.7-0.50-300--55` | 2.6 % · 5.0 | 95.8 % · 15.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.7-0.50-300--50` | 1.5 % · 3.0 | 93.5 % · 16.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.8-0.60-300--55` | 2.1 % · 4.0 | 95.6 % · 15.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.8-0.60-300--50` | 1.0 % · 2.0 | 93.3 % · 16.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.9-0.70-300--55` | 1.0 % · 2.0 | 95.1 % · 16.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.9-0.70-300--50` | 0.5 % · 1.0 | 93.2 % · 17.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |

**Noise only at -26 dBFS**, nobody talking: sent · spurts/min.

| Gate | keyboard | restaurant | street | white |
| --- | --- | --- | --- | --- |
| `energy-raw` | 96.6 % · 1.0 | 99.4 % · 1.0 | 99.9 % · 1.0 | 100.0 % · 1.0 |
| `energy-rnn` | 1.5 % · 3.0 | 93.3 % · 17.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `prob-0.6-0.40-300` | 4.4 % · 8.0 | 99.2 % · 5.0 | 1.1 % · 2.0 | 0.6 % · 1.0 |
| `prob-0.7-0.50-300` | 4.4 % · 8.0 | 98.9 % · 6.0 | 1.0 % · 2.0 | 0.5 % · 1.0 |
| `prob-0.8-0.60-300` | 3.3 % · 6.0 | 98.5 % · 8.0 | 1.0 % · 2.0 | 0.5 % · 1.0 |
| `prob-0.9-0.70-300` | 2.7 % · 5.0 | 97.4 % · 12.0 | 0.5 % · 1.0 | 0.5 % · 1.0 |
| `probfloor-0.6-0.40-300--55` | 3.3 % · 6.0 | 98.0 % · 8.0 | 0.5 % · 1.0 | 0.5 % · 1.0 |
| `probfloor-0.6-0.40-300--50` | 3.1 % · 6.0 | 97.5 % · 11.0 | 0.5 % · 1.0 | 0.5 % · 1.0 |
| `probfloor-0.7-0.50-300--55` | 3.3 % · 6.0 | 97.9 % · 10.0 | 0.5 % · 1.0 | 0.5 % · 1.0 |
| `probfloor-0.7-0.50-300--50` | 3.1 % · 6.0 | 97.5 % · 11.0 | 0.5 % · 1.0 | 0.5 % · 1.0 |
| `probfloor-0.8-0.60-300--55` | 2.7 % · 5.0 | 97.6 % · 11.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `probfloor-0.8-0.60-300--50` | 2.6 % · 5.0 | 96.9 % · 13.0 | 0.5 % · 1.0 | 0.0 % · 0.0 |
| `probfloor-0.9-0.70-300--55` | 2.1 % · 4.0 | 96.8 % · 14.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |
| `probfloor-0.9-0.70-300--50` | 1.6 % · 3.0 | 95.9 % · 16.0 | 0.0 % · 0.0 | 0.0 % · 0.0 |

**Every gate, condensed**: speech mixes at all SNRs averaged; noise only averaged over the keyboard, street and white noise (babble is speech, see the per-noise tables) at each level.

| Gate | Speech sent | Clipped | Clip p90 ms | Noise sent @ -36 | Noise sent @ -26 | Spurts/min @ -36 | Spurts/min @ -26 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `prob-0.5-0.50-200` | 99.4 % | 1.0 % | 15 | 2.0 % | 1.9 % | 5.7 | 5.3 |
| `prob-0.5-0.50-300` | 99.7 % | 0.6 % | 14 | 2.9 % | 2.8 % | 5.0 | 5.0 |
| `prob-0.5-0.30-200` | 99.5 % | 1.0 % | 15 | 2.1 % | 2.0 % | 5.7 | 5.3 |
| `prob-0.5-0.30-300` | 99.7 % | 0.6 % | 14 | 3.0 % | 2.9 % | 5.0 | 5.0 |
| `prob-0.5-0.10-200` | 99.5 % | 1.0 % | 15 | 2.2 % | 2.1 % | 5.7 | 5.3 |
| `prob-0.5-0.10-300` | 99.8 % | 0.6 % | 14 | 3.1 % | 2.9 % | 5.0 | 5.0 |
| `prob-0.6-0.60-200` | 99.4 % | 1.0 % | 16 | 1.5 % | 1.4 % | 4.3 | 3.7 |
| `prob-0.6-0.60-300` | 99.7 % | 0.6 % | 15 | 2.3 % | 2.0 % | 4.3 | 3.7 |
| `prob-0.6-0.40-200` | 99.4 % | 1.0 % | 16 | 1.6 % | 1.4 % | 4.3 | 3.7 |
| `prob-0.6-0.40-300` | 99.7 % | 0.6 % | 15 | 2.3 % | 2.0 % | 4.3 | 3.7 |
| `prob-0.6-0.20-200` | 99.4 % | 1.0 % | 16 | 1.6 % | 1.4 % | 4.3 | 3.7 |
| `prob-0.6-0.20-300` | 99.7 % | 0.6 % | 15 | 2.3 % | 2.0 % | 4.3 | 3.7 |
| `prob-0.7-0.70-200` | 99.3 % | 1.0 % | 16 | 1.3 % | 1.4 % | 3.7 | 3.7 |
| `prob-0.7-0.70-300` | 99.7 % | 0.8 % | 16 | 1.9 % | 2.0 % | 3.7 | 3.7 |
| `prob-0.7-0.50-200` | 99.3 % | 1.0 % | 16 | 1.3 % | 1.4 % | 3.7 | 3.7 |
| `prob-0.7-0.50-300` | 99.7 % | 0.8 % | 16 | 2.0 % | 2.0 % | 3.7 | 3.7 |
| `prob-0.7-0.30-200` | 99.4 % | 1.0 % | 16 | 1.4 % | 1.4 % | 3.7 | 3.7 |
| `prob-0.7-0.30-300` | 99.7 % | 0.8 % | 16 | 2.0 % | 2.0 % | 3.7 | 3.7 |
| `prob-0.8-0.80-200` | 99.2 % | 1.6 % | 18 | 0.7 % | 1.1 % | 2.0 | 3.0 |
| `prob-0.8-0.80-300` | 99.6 % | 1.4 % | 17 | 1.1 % | 1.6 % | 2.0 | 3.0 |
| `prob-0.8-0.60-200` | 99.2 % | 1.6 % | 18 | 0.7 % | 1.1 % | 2.0 | 3.0 |
| `prob-0.8-0.60-300` | 99.7 % | 1.4 % | 17 | 1.1 % | 1.6 % | 2.0 | 3.0 |
| `prob-0.8-0.40-200` | 99.3 % | 1.6 % | 17 | 0.7 % | 1.2 % | 2.0 | 3.0 |
| `prob-0.8-0.40-300` | 99.7 % | 1.4 % | 17 | 1.1 % | 1.7 % | 2.0 | 3.0 |
| `prob-0.9-0.90-200` | 98.9 % | 2.0 % | 19 | 0.4 % | 0.8 % | 1.0 | 2.3 |
| `prob-0.9-0.90-300` | 99.5 % | 1.8 % | 19 | 0.5 % | 1.2 % | 1.0 | 2.3 |
| `prob-0.9-0.70-200` | 99.0 % | 2.0 % | 19 | 0.4 % | 0.8 % | 1.0 | 2.3 |
| `prob-0.9-0.70-300` | 99.6 % | 1.8 % | 18 | 0.5 % | 1.2 % | 1.0 | 2.3 |
| `prob-0.9-0.50-200` | 99.1 % | 2.0 % | 19 | 0.4 % | 0.8 % | 1.0 | 2.3 |
| `prob-0.9-0.50-300` | 99.6 % | 1.8 % | 18 | 0.5 % | 1.2 % | 1.0 | 2.3 |
| `prob-0.95-0.95-200` | 98.7 % | 2.5 % | 22 | 0.2 % | 0.6 % | 0.7 | 1.7 |
| `prob-0.95-0.95-300` | 99.4 % | 2.3 % | 22 | 0.3 % | 0.8 % | 0.7 | 1.7 |
| `prob-0.95-0.75-200` | 98.8 % | 2.5 % | 22 | 0.2 % | 0.6 % | 0.7 | 1.7 |
| `prob-0.95-0.75-300` | 99.5 % | 2.1 % | 22 | 0.4 % | 0.9 % | 0.7 | 1.7 |
| `prob-0.95-0.55-200` | 98.9 % | 2.5 % | 22 | 0.3 % | 0.6 % | 0.7 | 1.7 |
| `prob-0.95-0.55-300` | 99.5 % | 2.1 % | 22 | 0.4 % | 0.9 % | 0.7 | 1.7 |
| `probfloor-0.5-0.30-300--60` | 99.7 % | 1.0 % | 17 | 1.9 % | 2.3 % | 3.7 | 4.0 |
| `probfloor-0.5-0.30-300--55` | 99.7 % | 1.4 % | 18 | 1.2 % | 1.7 % | 2.3 | 3.0 |
| `probfloor-0.5-0.30-300--50` | 99.6 % | 1.6 % | 20 | 0.7 % | 1.6 % | 1.3 | 3.0 |
| `probfloor-0.5-0.30-300--45` | 99.5 % | 2.0 % | 24 | 0.2 % | 0.9 % | 0.3 | 1.7 |
| `probfloor-0.6-0.40-300--60` | 99.7 % | 1.0 % | 17 | 1.9 % | 1.8 % | 3.7 | 3.3 |
| `probfloor-0.6-0.40-300--55` | 99.7 % | 1.4 % | 18 | 1.0 % | 1.4 % | 2.0 | 2.7 |
| `probfloor-0.6-0.40-300--50` | 99.6 % | 1.6 % | 20 | 0.7 % | 1.4 % | 1.3 | 2.7 |
| `probfloor-0.6-0.40-300--45` | 99.5 % | 2.0 % | 24 | 0.2 % | 0.9 % | 0.3 | 1.7 |
| `probfloor-0.7-0.50-300--60` | 99.7 % | 1.2 % | 18 | 1.4 % | 1.8 % | 2.7 | 3.3 |
| `probfloor-0.7-0.50-300--55` | 99.7 % | 1.4 % | 19 | 0.9 % | 1.4 % | 1.7 | 2.7 |
| `probfloor-0.7-0.50-300--50` | 99.6 % | 1.6 % | 21 | 0.5 % | 1.4 % | 1.0 | 2.7 |
| `probfloor-0.7-0.50-300--45` | 99.5 % | 2.0 % | 25 | 0.0 % | 0.9 % | 0.0 | 1.7 |
| `probfloor-0.8-0.60-300--60` | 99.6 % | 1.6 % | 18 | 0.9 % | 1.1 % | 1.7 | 2.0 |
| `probfloor-0.8-0.60-300--55` | 99.6 % | 1.6 % | 19 | 0.7 % | 1.1 % | 1.3 | 2.0 |
| `probfloor-0.8-0.60-300--50` | 99.6 % | 1.6 % | 21 | 0.3 % | 1.0 % | 0.7 | 2.0 |
| `probfloor-0.8-0.60-300--45` | 99.5 % | 2.0 % | 25 | 0.0 % | 0.7 % | 0.0 | 1.3 |
| `probfloor-0.9-0.70-300--60` | 99.5 % | 1.8 % | 19 | 0.5 % | 0.7 % | 1.0 | 1.3 |
| `probfloor-0.9-0.70-300--55` | 99.5 % | 1.8 % | 20 | 0.3 % | 0.7 % | 0.7 | 1.3 |
| `probfloor-0.9-0.70-300--50` | 99.5 % | 1.8 % | 22 | 0.2 % | 0.5 % | 0.3 | 1.0 |
| `probfloor-0.9-0.70-300--45` | 99.4 % | 2.7 % | 28 | 0.0 % | 0.5 % | 0.0 | 1.0 |
| `probfloor-0.95-0.75-300--60` | 99.5 % | 2.1 % | 22 | 0.4 % | 0.5 % | 0.7 | 1.0 |
| `probfloor-0.95-0.75-300--55` | 99.5 % | 2.1 % | 23 | 0.2 % | 0.5 % | 0.3 | 1.0 |
| `probfloor-0.95-0.75-300--50` | 99.4 % | 2.1 % | 24 | 0.2 % | 0.2 % | 0.3 | 0.3 |
| `probfloor-0.95-0.75-300--45` | 99.3 % | 3.1 % | 29 | 0.0 % | 0.2 % | 0.0 | 0.3 |
| `energy-raw` | 99.9 % | 0.0 % | 5 | 95.5 % | 98.9 % | 9.3 | 1.0 |
| `energy-rnn` | 99.2 % | 3.1 % | 31 | 0.0 % | 0.7 % | 0.0 | 1.3 |

**The default gate (`probfloor-0.9-0.70-300--50`) on each model**: speech at 10/5 dB (sent · clipped), noise only at -26 dBFS (sent per noise).

| Variant | Speech sent · clipped | keyboard | restaurant | street | white |
| --- | --- | --- | --- | --- | --- |
| `v02-avx` | 99.5 % · 1.2 % | 1.6 % | 95.9 % | 0.0 % | 0.0 % |
| `mumble-avx` | 99.6 % · 2.0 % | 2.5 % | 95.9 % | 0.5 % | 0.0 % |
| `main-avx` | 99.0 % · 7.4 % | 8.3 % | 6.5 % | 0.0 % | 0.0 % |
| `main-little-avx` | 98.7 % · 10.9 % | 0.0 % | 5.3 % | 0.0 % | 0.0 % |

### 44.1 kHz context: linear resampling in front of RNNoise (`v02-avx`, 10 dB SNR)

| Noise | Path | Noise attenuation dB | SI-SDR dB | Probability ≥ 0.6 agrees with direct |
| --- | --- | --- | --- | --- |
| keyboard | direct | 32.9 | 6.76 | 100.0 % |
| keyboard | linear | 33.4 | 6.76 | 99.6 % |
| keyboard | unresampled | 42.0 | -42.00 | 97.6 % |
| restaurant | direct | 7.9 | 6.80 | 100.0 % |
| restaurant | linear | 8.1 | 6.82 | 99.5 % |
| restaurant | unresampled | 8.5 | -44.39 | 96.8 % |
| street | direct | 38.9 | 7.90 | 100.0 % |
| street | linear | 39.9 | 7.90 | 99.8 % |
| street | unresampled | 30.4 | -42.29 | 98.2 % |

### Browser probes

|  | chromium 156.0.8078.4 | firefox 157.0 | webkit 27.2 |
| --- | --- | --- | --- |
| wasm SIMD | yes | yes | yes |
| MSTP on Window | yes | no | no |
| Default AudioContext rate (null sink) | 44100 | 44100 | 44100 |
| `noiseSuppression` a supported constraint | yes | yes | **no** |
| `voiceIsolation` a supported constraint | yes | no | no |
| `WebAssembly.Module` → worklet via `processorOptions` | yes | yes | yes |
| `WebAssembly.Module` → worklet via `port.postMessage` | yes | yes | yes |
| Worklet render ratio over 6 s, min–max of the runs (`currentFrame` ÷ the worklet's clock ÷ rate; 1 = real time) | 0.9996–1.0003 (8 runs) | 0.9998–1.0004 (8 runs) | 1.0000–1.0002 (8 runs) |
| 48 kHz context + `createMediaStreamSource` of the mic, peak over 500 ms | ok, peak 0.000 | ok, peak 0.100 | ok, peak 0.100 |
| 44.1 kHz context + `createMediaStreamSource`, peak over 500 ms | ok, peak 0.073 | ok, peak 0.100 | ok, peak 0.100 |
| Mic opened with NS off → `getSettings().noiseSuppression` | false | false | undefined |
| … after `applyConstraints({noiseSuppression: true})` | false | false | undefined |
| wasm fetch + compile + instantiate ms (`main-avx`, localhost) | 52.9 + 5.4 + 6.8 | 31.4 + 9.1 + 4.6 | 67.0 + 10.0 + 6.0 |

#### Chromium: does `applyConstraints({noiseSuppression})` change the audio?

The fake microphone plays keyboard alone at -36 dBFS for 20 s (auto gain on). Level of 2–9 s and of 12–19 s; the switch runs at 10 s, and the no-switch runs show the file's own drift.

| Run | Opened with NS | Switched to | `getSettings()` after | 2–9 s dBFS | 12–19 s dBFS |
| --- | --- | --- | --- | --- | --- |
| offThenOn | false | true | false | -25.02 | -25.42 |
| onThenOff | true | false | true | -25.17 | -24.32 |
| offNoSwitch | false | – | – | -25.02 | -25.42 |
| onNoSwitch | true | – | – | -25.17 | -24.32 |

### Chromium's own noise suppression vs RNNoise (`v02-avx`), same fake microphone

The fake device plays 5 s of keyboard (-38.2 dBFS), then speech + keyboard at 10 dB SNR. Echo cancellation and auto gain on, as `voiceCapture.ts` opens it today (`getSettings()`: auto gain true / true in the NS on / off recordings). Lead = 0.5–4.5 s, keyboard only.

| Condition | Lead noise dBFS | Attenuation vs input dB | Speech level change dB | Energy gate in the lead: spurts / sent |
| --- | --- | --- | --- | --- |
| Chromium, NS off | -27.0 | -11.2 | 2.8 | 1 / 62.5 % |
| Chromium, NS on | -26.1 | -12.1 | 2.9 | 1 / 55.5 % |
| NS off + RNNoise | -47.9 | 9.6 | 1.2 | 3 / 40.0 % |
| NS on + RNNoise | -32.9 | -5.3 | 2.5 | 2 / 48.3 % |

RNNoise's speech probability over the keyboard-only lead: max 0.993, ≥ 0.5 on 3.3 % of frames, ≥ 0.9 on 1.5 %.

