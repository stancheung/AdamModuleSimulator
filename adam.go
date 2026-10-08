package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const modbusAddr = "0.0.0.0:502"

// ServerStatus reports whether the Modbus listener is up.
type ServerStatus struct {
	Listening bool   `json:"listening"`
	Address   string `json:"address"`
	Error     string `json:"error"`
}

// AdamServer owns the coil array and the Modbus listener. coils is guarded by
// coilMu: it is written by the Modbus connection goroutines and the HTTP
// handlers, and read by the coil fragment poller.
// asciiRegs backs the ASCII-over-Modbus channel: the utility writes the
// command frame with FC16 to holding reg 10000 (wire start 9999) and reads
// the reply back with FC3.
const asciiRegBase = 9999 // wire start address of reg 10000
const asciiRegSize = 250  // bytes

type AdamServer struct {
	coilMu    sync.RWMutex
	coils     [18]int
	mu        sync.Mutex
	lis       net.Listener
	listening bool
	listenErr string
	asciiMu   sync.Mutex
	asciiRegs [asciiRegSize]byte
	identity  *moduleIdentity
}

func NewAdamServer() *AdamServer {
	adam := AdamServer{}
	for idx := range adam.coils {
		if idx < 12 {
			adam.coils[idx] = 1
		}
	}
	return &adam
}

// StartListening opens the Modbus port if it is not already open. Bind failures
// are recorded on the server instead of killing the web server.
func (adamServer *AdamServer) StartListening() ServerStatus {
	adamServer.mu.Lock()
	defer adamServer.mu.Unlock()
	if adamServer.listening {
		return adamServer.statusLocked()
	}
	lis, err := net.Listen("tcp", modbusAddr)
	if err != nil {
		adamServer.listenErr = err.Error()
		fmt.Printf("[-] Cannot listen on %s: %v\n", modbusAddr, err)
		return adamServer.statusLocked()
	}
	adamServer.lis = lis
	adamServer.listening = true
	adamServer.listenErr = ""
	fmt.Println("Listening on " + modbusAddr)
	go adamServer.serve(lis)
	return adamServer.statusLocked()
}

// StopListening closes the Modbus port. Already-accepted connections stay open.
func (adamServer *AdamServer) StopListening() ServerStatus {
	adamServer.mu.Lock()
	defer adamServer.mu.Unlock()
	if adamServer.lis != nil {
		adamServer.lis.Close()
		adamServer.lis = nil
	}
	adamServer.listening = false
	adamServer.listenErr = ""
	fmt.Println("Stopped listening on " + modbusAddr)
	return adamServer.statusLocked()
}

func (adamServer *AdamServer) serve(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			fmt.Printf("[-] Accept error: %v\n", err)
			continue
		}
		go adamServer.handleConn(conn)
	}
}

func (adamServer *AdamServer) PollCoils() [18]int {
	adamServer.coilMu.RLock()
	defer adamServer.coilMu.RUnlock()
	return adamServer.coils
}

// ForceCoil sets a coil from the web panel. Every point is forceable so the
// panel doubles as a manual override alongside the Modbus listener.
func (adamServer *AdamServer) ForceCoil(index int, value int) {
	if index < 0 || index >= len(adamServer.coils) {
		return
	}
	adamServer.coilMu.Lock()
	defer adamServer.coilMu.Unlock()
	adamServer.coils[index] = value
}

func (adamServer *AdamServer) Status() ServerStatus {
	adamServer.mu.Lock()
	defer adamServer.mu.Unlock()
	return adamServer.statusLocked()
}

func (adamServer *AdamServer) statusLocked() ServerStatus {
	return ServerStatus{
		Listening: adamServer.listening,
		Address:   modbusAddr,
		Error:     adamServer.listenErr,
	}
}

func (adamServer *AdamServer) handleConn(conn net.Conn) {
	addr := conn.RemoteAddr().String()
	defer conn.Close()

	fmt.Printf("[+] Connected by %s\n", addr)

	for {
		data := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := conn.Read(data)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			fmt.Println("[-] Client disconnected (clean)")
			return
		}

		data = data[:n]
		adamServer.processRecvData(data, conn)
	}
}

func (adamServer *AdamServer) processRecvData(data []byte, conn net.Conn) {
	if len(data) < 8 {
		fmt.Printf("[-] Short frame (%d bytes), ignored\n", len(data))
		return
	}

	var resp []byte
	var err error

	switch data[7] {
	case 1, 2:
		resp, err = adamServer.constructReadCoilStatus(data)
	case 3, 4:
		resp, err = adamServer.constructReadHoldingRegs(data)
	case 5:
		resp, err = adamServer.constructForceSingleCoilResp(data)
	case 15:
		resp, err = adamServer.constructForceMultiCoilsResp(data)
	case 16:
		resp, err = adamServer.constructPresetMultiRegsResp(data)
	default:
		return
	}

	if err != nil {
		fmt.Printf("[-] Malformed request (FC%d): %v\n", data[7], err)
		return
	}
	if _, err := conn.Write(resp); err != nil {
		fmt.Printf("Server write error: %v", err)
	}
}

func (adamServer *AdamServer) constructReadCoilStatus(data []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("FC1/FC2 frame shorter than 12 bytes")
	}
	transactionID := data[0:2]
	protoID := data[2:4]
	devAddr := []byte{data[6]}
	funcCode := []byte{data[7]}

	startIndex := int(data[9])
	totalPoints := int(data[11])

	if startIndex > 11 {
		startIndex -= 4
	}
	if startIndex < 0 || startIndex >= len(adamServer.coils) {
		return nil, fmt.Errorf("start address %d out of range", startIndex)
	}

	size := totalPoints
	if size > len(adamServer.coils)-startIndex {
		size = len(adamServer.coils) - startIndex
	}
	coilsArr := make([]int, size)

	adamServer.coilMu.RLock()
	copy(coilsArr, adamServer.coils[startIndex:startIndex+size])
	adamServer.coilMu.RUnlock()

	byteSize := []byte{byte((size + 7) / 8)}
	byteArray := make([]byte, int(byteSize[0]))

	for j := 0; j < size; j++ {
		byteIdx := j / 8
		bitPos := j % 8
		if coilsArr[j] == 1 {
			byteArray[byteIdx] |= 1 << bitPos
		}
	}

	pduLength := 3 + int(byteSize[0])
	response := append(transactionID, protoID...)
	response = append(response, byte(pduLength>>8), byte(pduLength&0xFF))
	response = append(response, devAddr...)
	response = append(response, funcCode...)
	response = append(response, byteSize...)
	response = append(response, byteArray...)

	return response, nil
}

// constructReadHoldingRegs serves FC3/FC4 reads of the ASCII reply window
// (holding reg 10000+, wire start 9999). Reads outside that window return
// zeros so generic Modbus clients don't error out.
func (adamServer *AdamServer) constructReadHoldingRegs(data []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("FC3/FC4 frame shorter than 12 bytes")
	}
	tid := data[0:2]
	proto := data[2:4]
	unit := data[6]
	fn := data[7]
	start := int(binary.BigEndian.Uint16(data[8:10]))
	count := int(binary.BigEndian.Uint16(data[10:12]))
	if count < 1 || count > 125 {
		return nil, fmt.Errorf("FC%d count %d out of range", fn, count)
	}

	payload := make([]byte, count*2)
	off := start - asciiRegBase
	if off >= 0 {
		adamServer.asciiMu.Lock()
		for i := 0; i < count*2; i++ {
			if off+i < asciiRegSize {
				payload[i] = adamServer.asciiRegs[off+i]
			}
		}
		adamServer.asciiMu.Unlock()
	}

	pduLen := 3 + len(payload)
	resp := append(append([]byte{}, tid...), proto...)
	resp = append(resp, byte(pduLen>>8), byte(pduLen&0xFF))
	resp = append(resp, unit, fn, byte(len(payload)))
	resp = append(resp, payload...)
	return resp, nil
}

// constructPresetMultiRegsResp handles FC16. Writes into the ASCII command
// window (reg 10000) execute the ADAM ASCII command and store the reply for
// the follow-up FC3 read.
func (adamServer *AdamServer) constructPresetMultiRegsResp(data []byte) ([]byte, error) {
	if len(data) < 13 {
		return nil, errors.New("FC16 frame shorter than 13 bytes")
	}
	tid := data[0:2]
	proto := data[2:4]
	unit := data[6]
	start := int(binary.BigEndian.Uint16(data[8:10]))
	byteCount := int(data[12])
	if len(data) < 13+byteCount {
		return nil, errors.New("FC16 frame truncated")
	}
	payload := data[13 : 13+byteCount]

	off := start - asciiRegBase
	if off >= 0 && off+byteCount <= asciiRegSize {
		adamServer.asciiMu.Lock()
		copy(adamServer.asciiRegs[off:], payload)
		adamServer.asciiMu.Unlock()

		if start == asciiRegBase {
			id := adamServer.identity
			if id == nil {
				id = &moduleIdentity{Name: "6050", FwVer: "4.17"}
			}
			// Modbus word-aligns odd-length ASCII payloads with NUL.
			cmd := strings.TrimRight(string(payload), "\x00")
			reply := handleASCII(id, cmd)
			adamServer.asciiMu.Lock()
			for i := range adamServer.asciiRegs {
				adamServer.asciiRegs[i] = 0
			}
			copy(adamServer.asciiRegs[:], reply)
			adamServer.asciiMu.Unlock()
		}
	}

	pduLen := 6
	resp := append(append([]byte{}, tid...), proto...)
	resp = append(resp, byte(pduLen>>8), byte(pduLen&0xFF))
	resp = append(resp, unit, 16)
	resp = append(resp, data[8], data[9], data[10], data[11])
	return resp, nil
}

func (adamServer *AdamServer) constructForceSingleCoilResp(data []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, errors.New("FC5 frame shorter than 12 bytes")
	}
	index := int(data[9]) - 4
	if index < 0 || index >= len(adamServer.coils) {
		return nil, fmt.Errorf("coil address %d out of range", index)
	}

	switch data[10] {
	case 255:
		adamServer.ForceCoil(index, 1)
	case 0:
		adamServer.ForceCoil(index, 0)
	default:
		return nil, fmt.Errorf("FC5 value %#x is neither 0x0000 nor 0xFF00", data[10])
	}
	return data, nil
}

func (adamServer *AdamServer) constructForceMultiCoilsResp(data []byte) ([]byte, error) {
	if len(data) < 13 {
		return nil, errors.New("FC15 frame shorter than 13 bytes")
	}
	startIndex := int(data[9])
	coilsToWrite := int(data[11])

	coilsData := data[13:]

	if startIndex > 11 {
		startIndex -= 4
	}
	if startIndex < 0 || startIndex >= len(adamServer.coils) {
		return nil, fmt.Errorf("start address %d out of range", startIndex)
	}
	if coilsToWrite < 0 || coilsToWrite > len(adamServer.coils)-startIndex {
		return nil, fmt.Errorf("writing %d coils at %d exceeds the coil space", coilsToWrite, startIndex)
	}

	bitsArr, err := bytesToBigEndianBitsSlice(coilsData)
	if err != nil {
		return nil, err
	}
	if coilsToWrite > len(bitsArr) {
		return nil, fmt.Errorf("byte count too small for %d coils", coilsToWrite)
	}

	adamServer.coilMu.Lock()
	for i := 0; i < coilsToWrite; i++ {
		adamServer.coils[startIndex+i] = bitsArr[i]
	}
	adamServer.coilMu.Unlock()

	return data[0:12], nil
}

func bytesToBigEndianBitsSlice(bytes []byte) ([]int, error) {
	size := len(bytes) * 8
	bitsArr := make([]int, size)
	idx := 0
	for _, b := range bytes {
		binStr := fmt.Sprintf("%08b", b)
		binStrSlice := strings.Split(binStr, "")
		for i := len(binStr) - 1; i >= 0; i-- {
			bit, err := strconv.Atoi(binStrSlice[i])
			if err != nil {
				return nil, err
			}
			bitsArr[idx] = bit
			idx++
		}
	}
	return bitsArr, nil
}
