package httpapi

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

func processRSS() (uint64, bool) {
	if err := getProcessMemoryInfo.Find(); err != nil {
		return 0, false
	}
	// PROCESS_MEMORY_COUNTERS: the counters following PageFaultCount are SIZE_T
	// on both Windows architectures; WorkingSetSize is the resident working set.
	var counters struct {
		Size                                     uint32
		PageFaults                               uint32
		PeakWorkingSet, WorkingSet               uintptr
		QuotaPeakPagedPool, QuotaPagedPool       uintptr
		QuotaPeakNonPagedPool, QuotaNonPagedPool uintptr
		Pagefile, PeakPagefile                   uintptr
	}
	counters.Size = uint32(unsafe.Sizeof(counters))
	result, _, _ := getProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	return uint64(counters.WorkingSet), result != 0
}
