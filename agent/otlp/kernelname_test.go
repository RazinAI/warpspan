package otlp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Mangled names are real kernels recorded from PyTorch 2.15 (cu132).
func TestKernelFrame(t *testing.T) {
	tests := []struct {
		name   string
		kernel string
		want   string
	}{
		{name: "not mangled (cuBLAS)", kernel: "ampere_sgemm_128x64_tn", want: "ampere_sgemm_128x64_tn"},
		{name: "extern C", kernel: "add", want: "add"},
		{name: "invalid mangling", kernel: "_Zgarbage", want: "_Zgarbage"},
		{name: "plain function", kernel: "_Z6vecAddPKfS0_Pfi", want: "vecAdd"},
		{
			name:   "reduce (Welford, from std)",
			kernel: "_ZN2at6native13reduce_kernelILi512ELi1ENS0_8ReduceOpIfNS0_10WelfordOpsIffiN4cuda3std3__44pairIffEEEEjfLi2ELi2EEEEEvT1_",
			want:   "at::native::reduce_kernel<512, 1, ReduceOp<float, WelfordOps<float, float, int, pair<float, float>>, unsigned int, float, 2, 2>>",
		},
		{
			name:   "reduce (mean)",
			kernel: "_ZN2at6native13reduce_kernelILi512ELi1ENS0_8ReduceOpIfNS0_7MeanOpsIffffEEjfLi4ELi4EEEEEvT1_",
			want:   "at::native::reduce_kernel<512, 1, ReduceOp<float, MeanOps<float, float, float, float>, unsigned int, float, 4, 4>>",
		},
		{
			name:   "elementwise div",
			kernel: "_ZN2at6native18elementwise_kernelILi128ELi2EZNS0_22gpu_kernel_impl_nocastINS0_13BinaryFunctorIfffNS0_15binary_internal10DivFunctorIfEEEEEEvRNS_18TensorIteratorBaseERKT_EUliE_EEviT1_",
			want:   "at::native::elementwise_kernel<128, 2, gpu_kernel_impl_nocast<BinaryFunctor<float, float, float, DivFunctor<float>>>::{lambda}>",
		},
		{
			name:   "elementwise add",
			kernel: "_ZN2at6native18elementwise_kernelILi128ELi2EZNS0_22gpu_kernel_impl_nocastINS0_15CUDAFunctor_addIfEEEEvRNS_18TensorIteratorBaseERKT_EUliE_EEviT1_",
			want:   "at::native::elementwise_kernel<128, 2, gpu_kernel_impl_nocast<CUDAFunctor_add<float>>::{lambda}>",
		},
		{
			name:   "sort",
			kernel: "_ZN2at6native20bitonicSortKVInPlaceILi2ELin1ELi16ELi16EflNS0_4GTOpIfLb1EEEjEEvNS_4cuda6detail10TensorInfoIT3_T6_EES8_S8_S8_NS6_IT4_S8_EES8_T5_",
			want:   "at::native::bitonicSortKVInPlace<2, -1, 16, 16, float, long, GTOp<float, true>, unsigned int>",
		},
		{
			name:   "GELU (nested lambdas)",
			kernel: "_ZN2at6native29vectorized_elementwise_kernelILi4EZZZNS0_18GeluCUDAKernelImplERNS_18TensorIteratorBaseENS0_8GeluTypeEENKUlvE0_clEvENKUlvE0_clEvEUlfE_St5arrayIPcLm2EELb0EEEviT0_T1_",
			want:   "at::native::vectorized_elementwise_kernel<4, GeluCUDAKernelImpl::{lambda}, array<char*, 2ul>, false>",
		},
		{
			name:   "randn (2.5k chars demangled)",
			kernel: "_ZN2at6native54_GLOBAL__N__8e11a5fb_21_DistributionNormal_cu_0c5b6e8543distribution_elementwise_grid_stride_kernelIfLi4EZNS0_9templates4cuda20normal_and_transformIffPNS_17CUDAGeneratorImplEZZZNS4_13normal_kernelIS7_EEvRKNS_10TensorBaseEddT_ENKUlvE_clEvENKUlvE0_clEvEUlfE_EEvRNS_18TensorIteratorBaseET1_T2_EUlP24curandStatePhilox4_32_10E_ZNS1_27distribution_nullary_kernelIff6float4S7_SM_SF_EEvSH_SJ_RKT3_T4_EUlifE_EEvlNS_15PhiloxCudaStateESI_SJ_",
			want:   "at::native::(anonymous namespace)::distribution_elementwise_grid_stride_kernel<…>",
		},
		{
			name:   "topk",
			kernel: "_ZN2at6native6sbtopk10gatherTopKIfjLi2ELb0EEEvNS_4cuda6detail10TensorInfoIKT_T0_EES8_S8_bS8_S8_NS5_IS6_S8_EES8_NS5_IlS8_EES8_PS6_",
			want:   "at::native::sbtopk::gatherTopK<float, unsigned int, 2, false>",
		},
		{
			name:   "softmax (anonymous namespace)",
			kernel: "_ZN43_GLOBAL__N__f71e7945_10_SoftMax_cu_9f978f6320softmax_warp_forwardIfffLi10ELb0ELb0ELi32EEEvPT0_PKT_iiiPKbib",
			want:   "(anonymous namespace)::softmax_warp_forward<float, float, float, 10, false, false, 32>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kernelFrame(tt.kernel)
			if got != tt.want {
				t.Errorf("kernelFrame(%s)\n got: %s\nwant: %s", tt.kernel, got, tt.want)
			}
			if len(got) > maxKernelFrame && !strings.HasSuffix(got, "<…>") {
				t.Errorf("frame longer than %d: %d", maxKernelFrame, len(got))
			}
		})
	}
}

// blowupName mangles f<A<int>, A<A<int> × w>, ...>: each level repeats the
// previous one w times through substitutions, so the demangled length grows as
// w^levels while the mangled name stays short.
func blowupName(levels, w int) string {
	var b strings.Builder
	b.WriteString("_Z1fI1AIiE") // substitutions: f=S_, A=S0_, A<int>=S1_
	for k := 1; k <= levels; k++ {
		b.WriteString("S0_I")
		for range w {
			fmt.Fprintf(&b, "S%d_", k)
		}
		b.WriteString("E")
	}
	b.WriteString("Evv")
	return b.String()
}

func TestKernelFrameExponentialName(t *testing.T) {
	name := blowupName(9, 14) // ~440 bytes, demangles to ~10^10 characters
	start := time.Now()
	got := kernelFrame(name)
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
	if len(got) > printBudget+len("…") {
		t.Errorf("frame of %d bytes: %.80s…", len(got), got)
	}
}

func BenchmarkKernelFrame(b *testing.B) {
	for b.Loop() {
		kernelFrame(longKernel)
	}
}

// longKernel is the randn kernel, the longest name in the test table.
const longKernel = "_ZN2at6native54_GLOBAL__N__8e11a5fb_21_DistributionNormal_cu_0c5b6e8543distribution_elementwise_grid_stride_kernelIfLi4EZNS0_9templates4cuda20normal_and_transformIffPNS_17CUDAGeneratorImplEZZZNS4_13normal_kernelIS7_EEvRKNS_10TensorBaseEddT_ENKUlvE_clEvENKUlvE0_clEvEUlfE_EEvRNS_18TensorIteratorBaseET1_T2_EUlP24curandStatePhilox4_32_10E_ZNS1_27distribution_nullary_kernelIff6float4S7_SM_SF_EEvSH_SJ_RKT3_T4_EUlifE_EEvlNS_15PhiloxCudaStateESI_SJ_"
