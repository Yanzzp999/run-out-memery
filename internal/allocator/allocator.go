package allocator

import (
	"crypto/rand"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	PageSize = 4096 // 4KB page size
)

// AllocatePhysicalMemory allocates memory and forces physical allocation
func AllocatePhysicalMemory(size int) ([]byte, error) {
	// Allocate the memory
	chunk := make([]byte, size)

	// Fill with random data to prevent compression and deduplication
	if _, err := rand.Read(chunk); err != nil {
		// Fallback: fill with varying patterns
		for i := 0; i < size; i++ {
			chunk[i] = byte(i % 256)
		}
	}

	// Force allocation of physical memory by writing to every page
	for i := 0; i < size; i += PageSize {
		// Write to the beginning of each page
		chunk[i] = byte((i / PageSize) % 256)

		// Write to middle of page if it exists
		if i+PageSize/2 < size {
			chunk[i+PageSize/2] = byte(((i / PageSize) + 128) % 256)
		}

		// Write to end of page if it exists
		if i+PageSize-1 < size {
			chunk[i+PageSize-1] = byte(((i / PageSize) + 255) % 256)
		}
	}

	// Try to lock memory to prevent swapping (may fail without privileges)
	ptr := uintptr(unsafe.Pointer(&chunk[0]))
	syscall.Syscall(syscall.SYS_MLOCK, ptr, uintptr(size), 0)

	// Force memory barrier to ensure all writes are committed
	runtime.KeepAlive(chunk)

	return chunk, nil
}
