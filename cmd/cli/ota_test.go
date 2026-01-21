package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// createTestUF2 creates a minimal valid UF2 file for testing
func createTestUF2(t *testing.T, numBlocks int) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.uf2")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i := 0; i < numBlocks; i++ {
		block := make([]byte, 512)

		// Magic numbers
		binary.LittleEndian.PutUint32(block[0:4], 0x0A324655)   // "UF2\n"
		binary.LittleEndian.PutUint32(block[4:8], 0x9E5D5157)   // Magic 2
		binary.LittleEndian.PutUint32(block[508:512], 0x0AB16F30) // Magic 3

		// Header
		binary.LittleEndian.PutUint32(block[8:12], 0x00002000)  // Flags (FAMILY_ID_PRESENT)
		binary.LittleEndian.PutUint32(block[12:16], 0x10000000) // Target address
		binary.LittleEndian.PutUint32(block[16:20], 256)        // Payload size
		binary.LittleEndian.PutUint32(block[20:24], uint32(i))  // Block number
		binary.LittleEndian.PutUint32(block[24:28], uint32(numBlocks)) // Total blocks
		binary.LittleEndian.PutUint32(block[28:32], 0xe48bff59) // Family ID (RP2350 RISC-V)

		// Fill payload with test pattern
		for j := 32; j < 32+256; j++ {
			block[j] = byte(i ^ j)
		}

		f.Write(block)
	}

	return path
}

func TestReadFirmwareInfo_ValidUF2(t *testing.T) {
	path := createTestUF2(t, 100)

	err := readFirmwareInfo(path)
	if err != nil {
		t.Errorf("readFirmwareInfo failed: %v", err)
	}
}

func TestReadFirmwareInfo_InvalidMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.uf2")

	// Create file with invalid magic
	data := make([]byte, 512)
	data[0] = 'N'
	data[1] = 'O'
	data[2] = 'P'
	data[3] = 'E'
	os.WriteFile(path, data, 0644)

	err := readFirmwareInfo(path)
	if err == nil {
		t.Error("expected error for invalid magic")
	}
}

func TestReadFirmwareInfo_TooSmall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.uf2")

	// Create file smaller than 512 bytes
	data := make([]byte, 100)
	os.WriteFile(path, data, 0644)

	err := readFirmwareInfo(path)
	if err == nil {
		t.Error("expected error for file too small")
	}
}

func TestReadFirmwareInfo_FileNotFound(t *testing.T) {
	err := readFirmwareInfo("/nonexistent/file.uf2")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestUF2FamilyIDs(t *testing.T) {
	tests := []struct {
		familyID uint32
		name     string
	}{
		{0xe48bff56, "RP2040"},
		{0xe48bff57, "RP2350 ARM-S"},
		{0xe48bff58, "RP2350 ARM-NS"},
		{0xe48bff59, "RP2350 RISC-V"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.uf2")

			block := make([]byte, 512)
			binary.LittleEndian.PutUint32(block[0:4], 0x0A324655)
			binary.LittleEndian.PutUint32(block[4:8], 0x9E5D5157)
			binary.LittleEndian.PutUint32(block[508:512], 0x0AB16F30)
			binary.LittleEndian.PutUint32(block[8:12], 0x00002000) // FAMILY_ID_PRESENT
			binary.LittleEndian.PutUint32(block[16:20], 256)
			binary.LittleEndian.PutUint32(block[24:28], 1)
			binary.LittleEndian.PutUint32(block[28:32], tc.familyID)

			os.WriteFile(path, block, 0644)

			err := readFirmwareInfo(path)
			if err != nil {
				t.Errorf("failed for family %s: %v", tc.name, err)
			}
		})
	}
}

func TestUF2BlockStructure(t *testing.T) {
	// Test that our UF2 creation matches the spec
	path := createTestUF2(t, 10)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(data) != 10*512 {
		t.Errorf("expected %d bytes, got %d", 10*512, len(data))
	}

	// Check first block
	block := data[0:512]

	magic1 := binary.LittleEndian.Uint32(block[0:4])
	if magic1 != 0x0A324655 {
		t.Errorf("magic1: expected 0x0A324655, got 0x%08x", magic1)
	}

	magic2 := binary.LittleEndian.Uint32(block[4:8])
	if magic2 != 0x9E5D5157 {
		t.Errorf("magic2: expected 0x9E5D5157, got 0x%08x", magic2)
	}

	magic3 := binary.LittleEndian.Uint32(block[508:512])
	if magic3 != 0x0AB16F30 {
		t.Errorf("magic3: expected 0x0AB16F30, got 0x%08x", magic3)
	}

	numBlocks := binary.LittleEndian.Uint32(block[24:28])
	if numBlocks != 10 {
		t.Errorf("numBlocks: expected 10, got %d", numBlocks)
	}

	// Check last block has correct block number
	lastBlock := data[9*512 : 10*512]
	blockNo := binary.LittleEndian.Uint32(lastBlock[20:24])
	if blockNo != 9 {
		t.Errorf("last block number: expected 9, got %d", blockNo)
	}
}

func TestOTAChunkSize(t *testing.T) {
	// Verify chunk size constant
	if otaChunkSize != 4096 {
		t.Errorf("expected chunk size 4096, got %d", otaChunkSize)
	}

	// Chunk size should be a multiple of UF2 block size (512)
	if otaChunkSize%512 != 0 {
		t.Errorf("chunk size should be multiple of 512")
	}
}

func TestExtractUF2Binary_ValidFile(t *testing.T) {
	// Create a test UF2 with sequential addresses
	numBlocks := 10
	payloadSize := 256
	baseAddr := uint32(0x10000000)

	uf2Data := make([]byte, numBlocks*512)
	for i := 0; i < numBlocks; i++ {
		block := uf2Data[i*512 : (i+1)*512]

		// Magic numbers
		binary.LittleEndian.PutUint32(block[0:4], 0x0A324655)
		binary.LittleEndian.PutUint32(block[4:8], 0x9E5D5157)
		binary.LittleEndian.PutUint32(block[508:512], 0x0AB16F30)

		// Header
		binary.LittleEndian.PutUint32(block[8:12], 0x00002000)                       // Flags
		binary.LittleEndian.PutUint32(block[12:16], baseAddr+uint32(i*payloadSize))  // Target address
		binary.LittleEndian.PutUint32(block[16:20], uint32(payloadSize))             // Payload size
		binary.LittleEndian.PutUint32(block[20:24], uint32(i))                       // Block number
		binary.LittleEndian.PutUint32(block[24:28], uint32(numBlocks))               // Total blocks
		binary.LittleEndian.PutUint32(block[28:32], 0xe48bff59)                      // Family ID

		// Fill payload with recognizable pattern
		for j := 0; j < payloadSize; j++ {
			block[32+j] = byte(i*10 + j%10)
		}
	}

	// Extract binary
	output, err := extractUF2Binary(uf2Data)
	if err != nil {
		t.Fatalf("extractUF2Binary failed: %v", err)
	}

	// Check output size
	expectedSize := numBlocks * payloadSize
	if len(output) != expectedSize {
		t.Errorf("expected %d bytes, got %d", expectedSize, len(output))
	}

	// Verify the data pattern was extracted correctly
	for i := 0; i < numBlocks; i++ {
		for j := 0; j < payloadSize; j++ {
			expected := byte(i*10 + j%10)
			actual := output[i*payloadSize+j]
			if actual != expected {
				t.Errorf("block %d offset %d: expected 0x%02x, got 0x%02x", i, j, expected, actual)
				return
			}
		}
	}
}

func TestExtractUF2Binary_NonSequentialBlocks(t *testing.T) {
	// Create UF2 with non-sequential block addresses (like real firmware)
	// This tests that we correctly handle address gaps
	payloadSize := uint32(256)

	// Create 3 blocks at addresses: 0x1000, 0x2000, 0x3000 (with gaps)
	uf2Data := make([]byte, 3*512)
	addresses := []uint32{0x10001000, 0x10002000, 0x10003000}

	for i := 0; i < 3; i++ {
		block := uf2Data[i*512 : (i+1)*512]

		binary.LittleEndian.PutUint32(block[0:4], 0x0A324655)
		binary.LittleEndian.PutUint32(block[4:8], 0x9E5D5157)
		binary.LittleEndian.PutUint32(block[508:512], 0x0AB16F30)

		binary.LittleEndian.PutUint32(block[8:12], 0x00002000)
		binary.LittleEndian.PutUint32(block[12:16], addresses[i])
		binary.LittleEndian.PutUint32(block[16:20], payloadSize)
		binary.LittleEndian.PutUint32(block[20:24], uint32(i))
		binary.LittleEndian.PutUint32(block[24:28], 3)
		binary.LittleEndian.PutUint32(block[28:32], 0xe48bff59)

		// Fill with marker byte for each block
		for j := 0; j < int(payloadSize); j++ {
			block[32+j] = byte(0xA0 + i)
		}
	}

	output, err := extractUF2Binary(uf2Data)
	if err != nil {
		t.Fatalf("extractUF2Binary failed: %v", err)
	}

	// Output should span from 0x10001000 to 0x10003100 = 0x2100 bytes
	expectedSize := addresses[2] + payloadSize - addresses[0]
	if uint32(len(output)) != expectedSize {
		t.Errorf("expected %d bytes, got %d", expectedSize, len(output))
	}

	// Check that first block's data is at offset 0
	if output[0] != 0xA0 {
		t.Errorf("first block marker: expected 0xA0, got 0x%02x", output[0])
	}

	// Check third block's data is at correct offset
	offset3 := addresses[2] - addresses[0]
	if output[offset3] != 0xA2 {
		t.Errorf("third block marker: expected 0xA2, got 0x%02x", output[offset3])
	}
}

func TestExtractUF2Binary_InvalidMagic(t *testing.T) {
	uf2Data := make([]byte, 512)
	uf2Data[0] = 'N' // Invalid magic

	_, err := extractUF2Binary(uf2Data)
	if err == nil {
		t.Error("expected error for invalid magic")
	}
}

func TestExtractUF2Binary_TooSmall(t *testing.T) {
	uf2Data := make([]byte, 100) // Less than 512 bytes

	_, err := extractUF2Binary(uf2Data)
	if err == nil {
		t.Error("expected error for file too small")
	}
}

func TestExtractUF2Binary_NotMultipleOf512(t *testing.T) {
	uf2Data := make([]byte, 600) // Not a multiple of 512

	_, err := extractUF2Binary(uf2Data)
	if err == nil {
		t.Error("expected error for invalid size")
	}
}
