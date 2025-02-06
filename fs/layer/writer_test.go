package layer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBufferedWriter(t *testing.T) {
	// Create temp directory for test files
	tempDir, err := os.MkdirTemp("", "buffered-writer-test")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	t.Run("writes after buffer size exceeded", func(t *testing.T) {
		filename := filepath.Join(tempDir, "test1.log")
		writer, err := NewBufferedWriter(filename, 100, 1*time.Hour, 1) // Long flush interval
		if err != nil {
			t.Fatalf("Failed to create writer: %v", err)
		}
		defer writer.Close()

		info := &RecorderInfo{
			ImageTag:   "test-image",
			LayerSha:   "sha256:123",
			LayerIndex: 1,
		}

		// Write multiple messages to exceed buffer
		for i := 0; i < 5; i++ {
			writer.Write(info, "/path/to/file")

		}

		// Force flush
		writer.Flush()

		// Read and verify file contents
		entries := readLogFile(t, filename)
		if len(entries) != 5 {
			t.Errorf("Expected 5 entries, got %d", len(entries))
		}

		// Verify entry content
		for _, entry := range entries {
			if entry["image"] != "test-image" {
				t.Errorf("Expected image tag 'test-image', got '%s'", entry["image"])
			}
			if entry["path"] != "/path/to/file" {
				t.Errorf("Expected path '/path/to/file', got '%s'", entry["path"])
			}
		}
	})

	t.Run("writes after time interval", func(t *testing.T) {
		filename := filepath.Join(tempDir, "test2.log")
		writer, err := NewBufferedWriter(filename, 1024, 100*time.Millisecond, 2) // Short flush interval
		if err != nil {
			t.Fatalf("Failed to create writer: %v", err)
		}
		defer writer.Close()

		info := &RecorderInfo{
			ImageTag:   "test-image",
			LayerSha:   "sha256:123",
			LayerIndex: 1,
		}

		writer.Write(info, "/path/to/file")

		// Wait for automatic flush
		time.Sleep(150 * time.Millisecond)

		entries := readLogFile(t, filename)
		if len(entries) != 1 {
			t.Errorf("Expected 1 entry after interval, got %d", len(entries))
		}
	})

	t.Run("writes on close", func(t *testing.T) {
		filename := filepath.Join(tempDir, "test3.log")
		writer, err := NewBufferedWriter(filename, 1024, 1*time.Hour, 2) // Long flush interval
		if err != nil {
			t.Fatalf("Failed to create writer: %v", err)
		}

		info := &RecorderInfo{
			ImageTag:   "test-image",
			LayerSha:   "sha256:123",
			LayerIndex: 1,
		}

		writer.Write(info, "/path/to/file")

		// Check file is empty before close
		entries := readLogFile(t, filename)
		if len(entries) != 0 {
			t.Error("Expected no entries before close")
		}

		// Close writer and check if data was written
		writer.Close()

		entries = readLogFile(t, filename)
		if len(entries) != 1 {
			t.Errorf("Expected 1 entry after close, got %d", len(entries))
		}
	})

	t.Run("appends to existing file", func(t *testing.T) {
		filename := filepath.Join(tempDir, "test4.log")

		// Write initial content
		writer1, err := NewBufferedWriter(filename, 100, time.Hour, 2)
		if err != nil {
			t.Fatalf("Failed to create first writer: %v", err)
		}

		info1 := &RecorderInfo{
			ImageTag:   "image1",
			LayerSha:   "sha256:123",
			LayerIndex: 1,
		}

		writer1.Write(info1, "/path1")
		if err != nil {
			t.Fatalf("First write failed: %v", err)
		}
		writer1.Close()

		// Create second writer for same file
		writer2, err := NewBufferedWriter(filename, 100, time.Hour, 2)
		if err != nil {
			t.Fatalf("Failed to create second writer: %v", err)
		}

		info2 := &RecorderInfo{
			ImageTag:   "image2",
			LayerSha:   "sha256:456",
			LayerIndex: 2,
		}

		writer2.Write(info2, "/path2")

		writer2.Close()

		// Verify both entries exist
		entries := readLogFile(t, filename)
		if len(entries) != 2 {
			t.Errorf("Expected 2 entries, got %d", len(entries))
		}

		if entries[0]["image"] != "image1" || entries[1]["image"] != "image2" {
			t.Error("Entries not in expected order or missing")
		}
	})
}

// Helper function to read and parse the log file
func readLogFile(t *testing.T, filename string) []map[string]string {
	t.Helper()

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("Failed to open file: %v", err)
	}
	defer file.Close()

	var entries []map[string]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry map[string]string
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("Failed to unmarshal entry: %v", err)
		}
		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Error reading file: %v", err)
	}

	return entries
}
