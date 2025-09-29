package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	chunkSize = 512 * 1024 * 1024 // 512MB in bytes
	pageSize  = 4096              // 4KB page size
)

type state int

const (
	stateInput state = iota
	stateAllocating
	stateDone
	stateError
)

type model struct {
	state          state
	input          string
	targetGB       float64
	targetBytes    int64
	allocatedBytes int64
	memoryChunks   [][]byte
	errorMsg       string
	startTime      time.Time
	cursor         int
	touchStopChan  chan bool
	touchWg        sync.WaitGroup
}

type tickMsg time.Time
type allocateMsg struct{}
type errorMsg string

// Initial model
func initialModel() model {
	return model{
		state:         stateInput,
		input:         "",
		memoryChunks:  make([][]byte, 0),
		touchStopChan: make(chan bool, 1),
	}
}

// Init command
func (m model) Init() tea.Cmd {
	return nil
}

// Update handles messages
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch m.state {
		case stateInput:
			return m.handleInputState(msg)
		case stateAllocating:
			return m.handleAllocatingState(msg)
		case stateDone, stateError:
			return m.handleDoneState(msg)
		}

	case allocateMsg:
		return m.handleAllocateMsg()

	case tickMsg:
		if m.state == stateAllocating {
			return m, tea.Batch(
				tickCmd(),
				func() tea.Msg { return allocateMsg{} },
			)
		}
		return m, nil

	case errorMsg:
		m.state = stateError
		m.errorMsg = string(msg)
		return m, nil
	}

	return m, nil
}

func (m model) handleInputState(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.stopMemoryTouching()
		return m, tea.Quit
	case "enter":
		if m.input == "" {
			return m, nil
		}

		gb, err := strconv.ParseFloat(m.input, 64)
		if err != nil || gb <= 0 {
			return m, func() tea.Msg {
				return errorMsg("请输入一个有效的正数（GB）")
			}
		}

		m.targetGB = gb
		m.targetBytes = int64(gb * 1024 * 1024 * 1024) // Convert GB to bytes
		m.state = stateAllocating
		m.startTime = time.Now()

		return m, tea.Batch(
			tickCmd(),
			func() tea.Msg { return allocateMsg{} },
		)

	case "backspace":
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	default:
		if msg.String() >= "0" && msg.String() <= "9" || msg.String() == "." {
			m.input += msg.String()
		}
	}
	return m, nil
}

func (m model) handleAllocatingState(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.stopMemoryTouching()
		return m, tea.Quit
	case "s":
		// Stop allocation
		m.state = stateDone
		return m, nil
	}
	return m, nil
}

func (m model) handleDoneState(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.stopMemoryTouching()
		return m, tea.Quit
	case "r":
		// Reset
		return initialModel(), nil
	}
	return m, nil
}

func (m model) handleAllocateMsg() (tea.Model, tea.Cmd) {
	if m.allocatedBytes >= m.targetBytes {
		m.state = stateDone
		// Start memory touching goroutine to prevent swapping
		m.touchWg.Add(1)
		go m.touchMemoryPeriodically()
		return m, nil
	}

	// Calculate how much to allocate in this chunk
	remaining := m.targetBytes - m.allocatedBytes
	var allocSize int64
	if remaining >= chunkSize {
		allocSize = chunkSize
	} else {
		allocSize = remaining
	}

	// Allocate memory and force physical allocation
	chunk, err := allocatePhysicalMemory(int(allocSize))
	if err != nil {
		return m, func() tea.Msg {
			return errorMsg(fmt.Sprintf("内存分配失败: %v", err))
		}
	}

	// Keep reference to prevent GC
	m.memoryChunks = append(m.memoryChunks, chunk)
	m.allocatedBytes += allocSize

	if m.allocatedBytes >= m.targetBytes {
		m.state = stateDone
		// Start memory touching goroutine to prevent swapping
		m.touchWg.Add(1)
		go m.touchMemoryPeriodically()
		return m, nil
	}

	return m, tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Millisecond*100, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// View renders the interface
func (m model) View() string {
	var s string

	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("86")).
		Bold(true).
		Padding(1, 2)

	inputStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("205")).
		Bold(true)

	progressStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("39")).
		Bold(true)

	errorStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Bold(true)

	successStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("46")).
		Bold(true)

	s += titleStyle.Render("🧠 内存消耗器 - Memory Eater")
	s += "\n\n"

	switch m.state {
	case stateInput:
		s += "请输入需要占用的内存大小（GB）:\n\n"
		s += inputStyle.Render(fmt.Sprintf("> %s", m.input))
		s += "\n\n"
		s += "按 Enter 开始分配，Ctrl+C 退出"

	case stateAllocating:
		allocatedGB := float64(m.allocatedBytes) / (1024 * 1024 * 1024)
		progress := float64(m.allocatedBytes) / float64(m.targetBytes) * 100
		elapsed := time.Since(m.startTime)

		s += progressStyle.Render(fmt.Sprintf("正在分配内存... %.2f%% 完成", progress))
		s += "\n\n"
		s += fmt.Sprintf("目标: %.2f GB\n", m.targetGB)
		s += fmt.Sprintf("已分配: %.2f GB\n", allocatedGB)
		s += fmt.Sprintf("内存块数量: %d\n", len(m.memoryChunks))
		s += fmt.Sprintf("用时: %v\n", elapsed.Truncate(time.Millisecond*10))

		// Show memory stats
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		s += fmt.Sprintf("堆内存使用: %.2f MB\n", float64(mem.Alloc)/(1024*1024))
		s += fmt.Sprintf("系统内存: %.2f MB\n", float64(mem.Sys)/(1024*1024))

		s += "\n"
		s += "按 's' 停止分配，Ctrl+C 退出"

	case stateDone:
		allocatedGB := float64(m.allocatedBytes) / (1024 * 1024 * 1024)
		elapsed := time.Since(m.startTime)

		s += successStyle.Render("✅ 内存分配完成!")
		s += "\n\n"
		s += fmt.Sprintf("目标: %.2f GB\n", m.targetGB)
		s += fmt.Sprintf("实际分配: %.2f GB\n", allocatedGB)
		s += fmt.Sprintf("内存块数量: %d\n", len(m.memoryChunks))
		s += fmt.Sprintf("总用时: %v\n", elapsed.Truncate(time.Millisecond*10))

		// Show final memory stats
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		s += fmt.Sprintf("堆内存使用: %.2f MB\n", float64(mem.Alloc)/(1024*1024))
		s += fmt.Sprintf("系统内存: %.2f MB\n", float64(mem.Sys)/(1024*1024))

		s += "\n"
		s += "内存将保持占用状态直到程序退出"
		s += "\n"
		s += "按 'r' 重新开始，Ctrl+C 退出"

	case stateError:
		s += errorStyle.Render("❌ 错误:")
		s += "\n\n"
		s += m.errorMsg
		s += "\n\n"
		s += "按 'r' 重新开始，Ctrl+C 退出"
	}

	return s
}

// stopMemoryTouching stops the memory touching goroutine
func (m *model) stopMemoryTouching() {
	select {
	case m.touchStopChan <- true:
	default:
	}
	m.touchWg.Wait()
}

// allocatePhysicalMemory allocates memory and forces physical allocation
func allocatePhysicalMemory(size int) ([]byte, error) {
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
	for i := 0; i < size; i += pageSize {
		// Write to the beginning of each page
		chunk[i] = byte((i / pageSize) % 256)

		// Write to middle of page if it exists
		if i+pageSize/2 < size {
			chunk[i+pageSize/2] = byte(((i / pageSize) + 128) % 256)
		}

		// Write to end of page if it exists
		if i+pageSize-1 < size {
			chunk[i+pageSize-1] = byte(((i / pageSize) + 255) % 256)
		}
	}

	// Try to lock memory to prevent swapping (may fail without privileges)
	ptr := uintptr(unsafe.Pointer(&chunk[0]))
	syscall.Syscall(syscall.SYS_MLOCK, ptr, uintptr(size), 0)

	// Force memory barrier to ensure all writes are committed
	runtime.KeepAlive(chunk)

	return chunk, nil
}

// touchMemoryPeriodically continuously accesses memory to prevent swapping
func (m *model) touchMemoryPeriodically() {
	defer m.touchWg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.touchStopChan:
			return
		case <-ticker.C:
			// Access random locations in all chunks to prevent swapping
			for chunkIdx, chunk := range m.memoryChunks {
				if len(chunk) == 0 {
					continue
				}

				// Access multiple random locations in each chunk
				for i := 0; i < 10; i++ {
					// Generate pseudo-random index based on chunk index and time
					// to avoid using crypto/rand which might be expensive
					seed := time.Now().UnixNano() + int64(chunkIdx*100+i)
					idx := int(seed % int64(len(chunk)))

					// Read and modify the value to ensure memory access
					val := chunk[idx]
					chunk[idx] = val ^ byte(seed%256)
				}

				// Also access page boundaries to ensure pages stay resident
				for pageStart := 0; pageStart < len(chunk); pageStart += pageSize * 100 { // Every 100 pages
					if pageStart < len(chunk) {
						chunk[pageStart] = chunk[pageStart] ^ 0x01
					}
				}
			}
		}
	}
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
