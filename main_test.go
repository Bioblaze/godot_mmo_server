package main

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
	"encoding/json"
	"os"
)

func TestConnection(t *testing.T) {
	// Start the server in a separate goroutine
	go func() {
		err := startServer()
		if err != nil {
			t.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Give the server some time to start up
	time.Sleep(2 * time.Second)

	// Connect to the server
	conn, err := net.Dial("tcp", "localhost:6000")
	if err != nil {
		t.Fatalf("Failed to connect to server: %v", err)
	}
	defer conn.Close()

	loginTest(t, conn, "testUser1")

	moveTest(t, conn, "testUser1");
}

func loginTest(t *testing.T, conn net.Conn, username string) {
// Send a username to the server
fmt.Fprintf(conn, username + "\n")

// Read server response
response, err := bufio.NewReader(conn).ReadString('\n')
if err != nil {
	t.Fatalf("Failed to read server response: %v", err)
}

// Parse the response
var mapUpdate [][]CellInfo
err = json.Unmarshal([]byte(response), &mapUpdate)
if err != nil {
	t.Fatalf("Failed to parse map update: %v", err)
}
fmt.Fprintf(os.Stdout, "loginTest(%s): PASSED", username)
}

func moveTest(t *testing.T, conn net.Conn, username string) {
	directionTest(t, conn, "south", 0, 1, username)
	directionTest(t, conn, "north", 0, 0, username)
	directionTest(t, conn, "east", 1, 0, username)
	directionTest(t, conn, "west", 0, 0, username)
}

func directionTest(t *testing.T, conn net.Conn, direction string, dx, dy int, username string) {
	// Send move command
	conn.Write([]byte("/" + direction + "\n"))

	// Read server response
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}

	// Parse the response
	var moveResponse struct {
		Action   string `json:"action"`
		Username string `json:"username"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	fmt.Printf("Server Response: %s", response)
	err = json.Unmarshal([]byte(response), &moveResponse)
	if err != nil {
		t.Fatalf("Failed to parse server response: %v", err)
	}

	// Check if the position is correct
	expectedX := 0 + dx
	expectedY := 0 + dy
	if moveResponse.Action != "move" || moveResponse.Username != username || moveResponse.X != expectedX || moveResponse.Y != expectedY {
		t.Fatalf("Unexpected move response: %+v", moveResponse)
	}
	fmt.Fprintf(os.Stdout, "moveTest(%s): PASSED\n", direction)
}
