package main

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
	"encoding/json"
	"os"
	"io"
	"net/http"
	"bytes"
	"errors"
	"github.com/golang-jwt/jwt"
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

	moveTest(t, conn, "testUser1")
}

func TestMovement(t *testing.T) {
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

	moveTest(t, conn, "testUser1")
}

func TestClientKickUser(t *testing.T) {
	// Start the server in a separate goroutine
	go func() {
		err := startServer()
		if err != nil {
			t.Fatalf("Failed to start server: %v", err)
		}
	}()
	go startAPI()

    // Connect two clients
    conn1 := connectClient(t)
    defer conn1.Close()
    conn2 := connectClient(t)
    defer conn2.Close()

    // Send valid JWT tokens for both clients

	loginTest(t, conn1, "testUser1")

	loginTest(t, conn2, "testUser2")

	// Read testUser1 has joined chat! from testUser1
	response_one, err := bufio.NewReader(conn1).ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}

	fmt.Println(response_one)

	// Give the clients some time to connect
	time.Sleep(5 * time.Second)


	kickReq := struct {
		Username string `json:"username"`
	}{
		Username: "testUser2",
	}

	kickReqBytes, err := json.Marshal(kickReq)
	if err != nil {
		t.Fatal("Failed to marshal kick request")
	}

	req, err := http.NewRequest("POST", "http://localhost:5000/api/kickUser", bytes.NewBuffer(kickReqBytes))
	if err != nil {
		t.Fatal("Failed to create kick request")
	}

	// Generate JWT and set RPG_AUTH header
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{})
	tokenString, err := token.SignedString([]byte(apijwtSecret))
	if err != nil {
		t.Fatal("Failed to sign JWT")
	}
	req.Header.Set("RPG_AUTH", tokenString)

	fmt.Println("Sending http call to kick user")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("Failed to execute kick request")
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	// Read kicked announcement from testUser1
	response, err := bufio.NewReader(conn1).ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}

	fmt.Println(response)

	var announcement struct {
		Action   string `json:"action"`
		Username string `json:"username"`
		Message  string `json:"message"`
	}
	err = json.Unmarshal([]byte(response), &announcement)
	if err != nil {
		t.Fatalf("Failed to parse server response: %v", err)
	}

	expectedMessage := "has been kicked from the server."
	if announcement.Action != "announcement" || announcement.Username != "testUser2" || announcement.Message != expectedMessage {
		t.Fatalf("Unexpected announcement: %+v", announcement)
	}

	// Read kicked announcement from testUser1
	response_after, err := bufio.NewReader(conn2).ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}

	// Parse the response
	var actionResponse struct {
		Action   string `json:"action"`
		Message string `json:"message"`
	}
	fmt.Printf("Server Response: %s", response)
	err = json.Unmarshal([]byte(response_after), &actionResponse)
	if err != nil {
		t.Fatalf("Failed to parse server response: %v", err)
	}
	if actionResponse.Action != "kicked" || actionResponse.Message != "You have been kicked." {
		t.Fatalf("Unexpected kicked response: %+v", actionResponse)
	}


	_, err2 := bufio.NewReader(conn2).ReadString('\n')
	if errors.Is(err2, io.EOF) {
		fmt.Println("TestClientKickUser: PASSED")
	} else if err2 != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}
}

func connectClient(t *testing.T) net.Conn {
    conn, err := net.Dial("tcp", "localhost:6000")
    if err != nil {
        t.Fatalf("Failed to connect to server: %v", err)
    }
    return conn
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
