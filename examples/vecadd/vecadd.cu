#include <cstdio>
#include <cuda_runtime.h>
#include <math.h>

#define CUDA_CHECK(call)                                                       \
  do {                                                                         \
    cudaError_t err_ = (call);                                                 \
    if (err_ != cudaSuccess) {                                                 \
      fprintf(stderr, "%s:%d: %s\n", __FILE__, __LINE__,                       \
              cudaGetErrorString(err_));                                       \
      exit(1);                                                                 \
    }                                                                          \
  } while (0)

__global__ void add(int N, float *x, float *y) {
  int thread_index = blockIdx.x * blockDim.x + threadIdx.x;
  int stride = blockDim.x * gridDim.x;

  for (int i = thread_index; i < N; i += stride) {
    y[i] = x[i] + y[i];
  }
}

int main() {
  int N = 1 << 20;

  float *x;
  float *y;

  CUDA_CHECK(cudaMallocManaged(&x, N * sizeof(float)));
  CUDA_CHECK(cudaMallocManaged(&y, N * sizeof(float)));

  for (int i = 0; i < N; i++) {
    x[i] = 1.0f;
    y[i] = 2.0f;
  }

  cudaMemLocation loc{};
  loc.type = cudaMemLocationTypeDevice;
  loc.id = 0;

  CUDA_CHECK(cudaMemPrefetchAsync(x, N * sizeof(float), loc, 0, 0));
  CUDA_CHECK(cudaMemPrefetchAsync(y, N * sizeof(float), loc, 0, 0));

  int num_threads = 256;
  int num_blocks = (N + num_threads - 1) / num_threads;

  add<<<num_blocks, num_threads>>>(N, x, y);

  CUDA_CHECK(cudaGetLastError());

  CUDA_CHECK(cudaDeviceSynchronize());

  CUDA_CHECK(cudaFree(x));
  CUDA_CHECK(cudaFree(y));

  return 0;
}
