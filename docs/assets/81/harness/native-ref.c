/* R80 NS0: native reference for the wasm parity check (docs/81 §12).
 * Reads 48 kHz mono f32le on stdin (±1), writes the denoised samples (±1)
 * to stdout and one VAD probability per 480-sample frame to argv[1]. */
#include <stdio.h>
#include "rnnoise.h"
int main(int argc, char **argv) {
  DenoiseState *st = rnnoise_create(NULL);
  FILE *vad = fopen(argv[1], "wb");
  float in[480], out[480];
  while (fread(in, sizeof(float), 480, stdin) == 480) {
    for (int i = 0; i < 480; i++) in[i] *= 32768.f;
    float p = rnnoise_process_frame(st, out, in);
    for (int i = 0; i < 480; i++) out[i] /= 32768.f;
    fwrite(out, sizeof(float), 480, stdout);
    fwrite(&p, sizeof(float), 1, vad);
  }
  fclose(vad);
  rnnoise_destroy(st);
  return 0;
}
