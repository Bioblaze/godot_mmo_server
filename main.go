package main

import (
	"bufio"
	"container/heap"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/golang-jwt/jwt"
)

type client struct {
	conn     net.Conn
	username string
	channel    *channel
	muted    bool
	x        int
	y        int
	commandRateLimiter *rateLimiter
	sleepDelay time.Duration
	mutedUsernames map[string]bool
}

type ClientInfo struct {
	Username string `json:"username"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
}

type CellType string

const jwtSecret = "your_jwt_secret"

type travelClaims struct {
	jwt.StandardClaims
	ServerName string `json:"server_name"`
	Username   string `json:"username"`
}

type rateLimiter struct {
	tokens           int
	maxTokens        int
	tokenFillRate    time.Duration
	lastCheck        time.Time
}

const (
	Empty CellType = iota
	Mountain
	Grass
	Water
)

type Cell struct {
	Type    CellType
	Clients sync.Map
}

type CellInfo struct {
	Type    CellType     `json:"type"`
	Clients []ClientInfo `json:"clients"`
}

type sessionPayload struct {
	ServerName string `json:"server_name"`
	Username   string `json:"username"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type LoadUserRequest struct {
	Username string `json:"username"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
}

type sendMessagePayload struct {
	FromUsername string `json:"from_username"`
	ToUsername   string `json:"to_username"`
	FromServer   string `json:"from_server"`
	Message      string `json:"message"`
}

type moveUserPayload struct {
	Username string `json:"username"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
}

var loadedUsers = make(map[string]LoadUserRequest)
var defaultSleepDelay = 3 * time.Second
const mapFilename = "map.json"
var grid [][]*Cell
var gridHeight = 25
var gridWidth = 25
const rpgAuthPassword = "your_predefined_password"


func decodeSessionToken(token string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", "", err
	}

	var payload sessionPayload
	err = json.Unmarshal(decoded, &payload)
	if err != nil {
		return "", "", err
	}

	if payload.ServerName == "" || payload.Username == "" {
		return "", "", errors.New("invalid session payload")
	}

	return payload.ServerName, payload.Username, nil
}

func initGrid() {
	grid = make([][]*Cell, gridHeight)
	for i := range grid {
		grid[i] = make([]*Cell, gridWidth)
		for j := range grid[i] {
			grid[i][j] = &Cell{
				Type:    Empty, // Assign the default type for now
				Clients: sync.Map{},
			}
		}
	}
}

type channel struct {
	name    string
	title   string
	clients sync.Map
}

var clients sync.Map
var channels sync.Map

func main() {
	go startAPI()
	// Check if the map file exists
	if _, err := os.Stat(mapFilename); os.IsNotExist(err) {
		// If the file does not exist, generate a new map
		initGrid()
	} else {
		// If the file exists, load the map from the file
		loadedGrid, err := loadMap(mapFilename)
		if err != nil {
			fmt.Printf("Error loading map from file: %v\n", err)
			return
		}
		grid = loadedGrid
	}
	// Set the maximum number of open files allowed by the system
	err := setMaxOpenFiles(2048)
	if err != nil {
		fmt.Printf("Error setting max open files limit: %v\n", err)
		return
	}

	ln, err := net.Listen("tcp", ":6000")
	if err != nil {
		panic(err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}

		go handleConnection(conn)
	}
}

func setMaxOpenFiles(limit uint64) error {
	var rLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return err
	}

	rLimit.Cur = limit
	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return err
	}

	return nil
}


func handleConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Error reading input:", err)
		return
	}

	

	input = input[:len(input)-1] // Remove the newline character

	// Try to decode the input as a session token
	serverName, username, err := decodeSessionToken(input)
	if err != nil {
		// If decoding fails, treat the input as a regular username
		username = input
	}

	cli := &client{
		conn:     conn,
		username: username,
		commandRateLimiter: newRateLimiter(5, time.Second),
		sleepDelay: defaultSleepDelay,
		mutedUsernames: make(map[string]bool),
	}
	clients.Store(cli.username, cli)
	if loadedUser, ok := loadedUsers[username]; ok {
		addToGridDirectly(cli, loadedUser.X, loadedUser.Y);
		delete(loadedUsers, username)
	} else {
		addToGridDirectly(cli, 0, 0);
	}
	//grid[0][0].Store(cli.username, cli) // Place the client at position (0, 0) by default
	announceMap(cli)

	if serverName != "" {
		announce(cli, fmt.Sprintf("transferred from %s and joined the chat!", serverName))
	} else {
		announce(cli, "joined the chat!")
	}

	defer func() {
		clients.Delete(cli.username)
		announce(cli, "left the chat!")
	}()

	for {
		msg, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		// Check if the message starts with the command prefix
		if len(msg) > 0 && msg[0] == '/' {
			handleCommand(cli, msg)
		} else {
			echo(cli, msg)
		}
	}
}

func announce(cli *client, action string) {
	msg := fmt.Sprintf("%s %s\n", cli.username, action)

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		if client.username != cli.username {
			client.conn.Write([]byte(msg))
		}
		return true
	})
}



func echo(cli *client, msg string) {
	if cli.muted {
		cli.conn.Write([]byte("You are muted and cannot send messages.\n"))
		return
	}

	if cli.channel != nil {
		chatChannel(cli, msg)
	} else {
		response := fmt.Sprintf("%s: %s", cli.username, msg)
		cli.conn.Write([]byte(response))
	}
}

func broadcast(cli *client) {
	msg := fmt.Sprintf("%s has requested a broadcast!\n", cli.username)

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		client.conn.Write([]byte(msg))
		return true
	})
}


func handleCommand(cli *client, msg string) {
	if !cli.commandRateLimiter.isAllowed() {
		cli.conn.Write([]byte("You are sending commands too fast. Please slow down.\n"))
		return
	}
	// Remove the newline character and command prefix
	msg = msg[1 : len(msg)-1]

	args := strings.Split(msg, " ")
	command := args[0]

	switch command {
	case "broadcast":
		broadcast(cli)
	case "msg":
		if len(args) < 3 {
			cli.conn.Write([]byte("Usage: /msg [username] [message]\n"))
		} else {
			targetUsername := args[1]
			message := strings.Join(args[2:], " ")
			privateMessage(cli, targetUsername, message)
		}
	case "list":
		listUsers(cli)
	case "mute":
		if len(args) < 2 {
			cli.conn.Write([]byte("Usage: /mute [username]\n"))
		} else {
			mute(cli, args)
		}
	case "unmute":
		if len(args) < 2 {
			cli.conn.Write([]byte("Usage: /unmute [username]\n"))
		} else {
			unmute(cli, args)
		}
	case "create":
		if len(args) < 2 {
			cli.conn.Write([]byte("Usage: /create [channel_name]\n"))
		} else {
			channelName := args[1]
			createChannel(cli, channelName)
		}
	case "join":
		if len(args) < 2 {
			cli.conn.Write([]byte("Usage: /join [channel_name]\n"))
		} else {
			channelName := args[1]
			joinChannel(cli, channelName)
		}
	case "part":
		partChannel(cli)
	case "setChannelTitle":
		if len(args) < 2 {
			cli.conn.Write([]byte("Usage: /setChannelTitle [title]\n"))
		} else {
			title := strings.Join(args[1:], " ")
			setChannelTitle(cli, title)
		}
	case "north":
		moveClient(cli, 0, -1)
	case "east":
		moveClient(cli, 1, 0)
	case "south":
		moveClient(cli, 0, 1)
	case "west":
		moveClient(cli, -1, 0)
	case "travel":
		jwt, err := generateJWT("current_server_name", cli.username)
		if err != nil {
			cli.conn.Write([]byte("Error generating travel token.\n"))
			return
		}
		response := fmt.Sprintf("Travel token: %s\n", jwt)
		cli.conn.Write([]byte(response))
	case "whisper":
		if len(args) < 3 {
			cli.conn.Write([]byte("Usage: /whisper [username] [message]\n"))
		} else {
			targetUsername := args[1]
			message := strings.Join(args[2:], " ")
			whisper(cli, targetUsername, message)
		}
	case "moveTo":
		if len(args) < 3 {
			cli.conn.Write([]byte("Usage: /moveTo [x] [y]\n"))
		} else {
			x, err1 := strconv.Atoi(args[1])
			y, err2 := strconv.Atoi(args[2])
			if err1 != nil || err2 != nil {
				cli.conn.Write([]byte("Invalid coordinates. Please enter integers.\n"))
			} else {
				moveTo(cli, x, y, cli.sleepDelay)
			}
		}
	case "help":
		help(cli)
	default:
		response := fmt.Sprintf("Unknown command: /%s\n", msg)
		cli.conn.Write([]byte(response))
	}
}
func listUsers(cli *client) {
	cli.conn.Write([]byte("Connected users:\n"))

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		cli.conn.Write([]byte(fmt.Sprintf(" - %s\n", client.username)))
		return true
	})
}

func privateMessage(cli *client, targetUsername, message string) {
	targetClient, ok := clients.Load(targetUsername)
	if !ok {
		response := fmt.Sprintf("User '%s' not found.\n", targetUsername)
		cli.conn.Write([]byte(response))
		return
	}

	response := fmt.Sprintf("(Private) %s: %s\n", cli.username, message)
	targetClient.(*client).conn.Write([]byte(response))
}

func muteUserGlobal(cli *client, targetUsername string) {
	targetClient, ok := clients.Load(targetUsername)
	if !ok {
		response := fmt.Sprintf("User '%s' not found.\n", targetUsername)
		cli.conn.Write([]byte(response))
		return
	}

	targetClient.(*client).muted = true
	response := fmt.Sprintf("You have muted '%s'.\n", targetUsername)
	cli.conn.Write([]byte(response))
}

func unmuteUserGlobal(cli *client, targetUsername string) {
	targetClient, ok := clients.Load(targetUsername)
	if !ok {
		response := fmt.Sprintf("User '%s' not found.\n", targetUsername)
		cli.conn.Write([]byte(response))
		return
	}

	targetClient.(*client).muted = false
	response := fmt.Sprintf("You have unmuted '%s'.\n", targetUsername)
	cli.conn.Write([]byte(response))
}



func createChannel(cli *client, channelName string) {
	_, ok := channels.Load(channelName)
	if ok {
		cli.conn.Write([]byte("Channel already exists.\n"))
		return
	}

	newChannel := &channel{
		name: channelName,
	}
	channels.Store(channelName, newChannel)
	response := fmt.Sprintf("Channel '%s' created.\n", channelName)
	cli.conn.Write([]byte(response))
}

func joinChannel(cli *client, channelName string) {
	newChannel, ok := channels.Load(channelName)
	if !ok {
		cli.conn.Write([]byte("Channel not found.\n"))
		return
	}

	if cli.channel != nil {
		cli.channel.clients.Delete(cli.username)
	}
	newChannel.(*channel).clients.Store(cli.username, cli)
	cli.channel = newChannel.(*channel)
	response := fmt.Sprintf("You have joined the channel '%s'.\n", channelName)
	cli.conn.Write([]byte(response))
}

func partChannel(cli *client) {
	if cli.channel == nil {
		cli.conn.Write([]byte("You are not in any channel.\n"))
		return
	}

	channelName := cli.channel.name
	cli.channel.clients.Delete(cli.username)
	cli.channel = nil
	response := fmt.Sprintf("You have left the channel '%s'.\n", channelName)
	cli.conn.Write([]byte(response))
}

func chatChannel(cli *client, msg string) {
	if cli.channel == nil {
		cli.conn.Write([]byte("You are not in any channel.\n"))
		return
	}

	response := fmt.Sprintf("[#%s] %s: %s", cli.channel.name, cli.username, msg)
	cli.channel.clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		if client.username != cli.username {
			client.conn.Write([]byte(response))
		}
		return true
	})
}

func setChannelTitle(cli *client, title string) {
	if cli.channel == nil {
		cli.conn.Write([]byte("You are not in any channel.\n"))
		return
	}

	cli.channel.title = title
	response := fmt.Sprintf("Channel title set to '%s'.\n", title)
	cli.conn.Write([]byte(response))
}

func moveClient(cli *client, dx, dy int) {
	newX, newY := cli.x+dx, cli.y+dy

	if newX < 0 || newX >= gridWidth || newY < 0 || newY >= gridHeight {
		sendJSON(cli.conn, map[string]interface{}{
			"type": "error",
			"msg":  "You cannot move outside the grid",
		})
		return
	}

	newCell := grid[newY][newX]
	switch newCell.Type {
	case Empty:
		removeFromGrid(cli)
		cli.x, cli.y = newX, newY
		addToGrid(cli)
		response := struct {
			Action string `json:"action"`
			X      int    `json:"x"`
			Y      int    `json:"y"`
		}{
			Action: "move",
			X:      newX,
			Y:      newY,
		}
	
		jsonResponse, err := json.Marshal(response)
		if err != nil {
			cli.conn.Write([]byte("Error generating move response.\n"))
			return
		}
	
		cli.conn.Write(append(jsonResponse, '\n'))
		broadcastLocation(cli)
	case Mountain:
		sendJSON(cli.conn, map[string]interface{}{
			"type": "error",
			"msg":  "You cannot move onto a mountain",
		})
	default:
		sendJSON(cli.conn, map[string]interface{}{
			"type": "error",
			"msg":  "You cannot move to that location",
		})
	}
}

func removeFromGrid(cli *client) {
	cell := grid[cli.y][cli.x]
	cell.Clients.Delete(cli.username)
}

func addToGrid(cli *client) {
	cell := grid[cli.y][cli.x]
	cell.Clients.Store(cli.username, cli)
}

func addToGridDirectly(cli *client, x int, y int) {
	cell := grid[y][x]
	cell.Clients.Store(cli.username, cli)
}

func announceMap(cli *client) {
	gridInfo := make([][]CellInfo, len(grid))
	for i := range grid {
		gridInfo[i] = make([]CellInfo, len(grid[i]))
		for j := range grid[i] {
			cellInfo := CellInfo{
				Type:    grid[i][j].Type,
				Clients: []ClientInfo{},
			}

			grid[i][j].Clients.Range(func(_, v interface{}) bool {
				client := v.(*client)
				cellInfo.Clients = append(cellInfo.Clients, ClientInfo{
					Username: client.username,
					X:        client.x,
					Y:        client.y,
				})
				return true
			})

			gridInfo[i][j] = cellInfo
		}
	}

	jsonData, err := json.Marshal(gridInfo)
	if err != nil {
		fmt.Println("Error marshaling grid data:", err)
		return
	}

	cli.conn.Write(append(jsonData, '\n'))
}

func generateJWT(serverName, username string) (string, error) {
	claims := &travelClaims{
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: time.Now().Add(time.Hour * 1).Unix(),
		},
		ServerName: serverName,
		Username:   username,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(jwtSecret))
}

func newRateLimiter(maxTokens int, fillRate time.Duration) *rateLimiter {
	return &rateLimiter{
		tokens:           maxTokens,
		maxTokens:        maxTokens,
		tokenFillRate:    fillRate,
		lastCheck:        time.Now(),
	}
}

func (rl *rateLimiter) isAllowed() bool {
	now := time.Now()
	elapsedTime := now.Sub(rl.lastCheck)
	rl.tokens += int(elapsedTime / rl.tokenFillRate)
	if rl.tokens > rl.maxTokens {
		rl.tokens = rl.maxTokens
	}
	rl.lastCheck = now

	if rl.tokens > 0 {
		rl.tokens--
		return true
	}

	return false
}

func broadcastLocation(cli *client) {
	response := struct {
		Action   string `json:"action"`
		Username string `json:"username"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}{
		Action:   "user_moved",
		Username: cli.username,
		X:        cli.x,
		Y:        cli.y,
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		return
	}

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		if client.username != cli.username {
			client.conn.Write(append(jsonResponse, '\n'))
		}
		return true
	})
}

type cellInfo struct {
	X, Y int
}

type priorityQueueItem struct {
	value    cellInfo
	priority int
	index    int
}

type priorityQueue []*priorityQueueItem

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	return pq[i].priority < pq[j].priority
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*priorityQueueItem)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

func aStarPathfinding(start, target cellInfo, grid *[][]*Cell) []cellInfo {
	// Initialize the priority queue with the starting position
	pq := &priorityQueue{}
	heap.Init(pq)
	heap.Push(pq, &priorityQueueItem{value: start, priority: 0})

	// Create maps to store the cost to move to a cell and the path from the starting cell
	costs := make(map[cellInfo]int)
	costs[start] = 0
	from := make(map[cellInfo]cellInfo)

	// Define a helper function to calculate the heuristic (Manhattan distance) between two cells
	heuristic := func(a, b cellInfo) int {
		return abs(a.X-b.X) + abs(a.Y-b.Y)
	}

	for pq.Len() > 0 {
		current := heap.Pop(pq).(*priorityQueueItem).value

		// If the target is reached, build the path and return it
		if current == target {
			path := []cellInfo{}
			for current != start {
				path = append([]cellInfo{current}, path...)
				current = from[current]
			}
			return path
		}

		neighbors := []cellInfo{
			{current.X - 1, current.Y},
			{current.X + 1, current.Y},
			{current.X, current.Y - 1},
			{current.X, current.Y + 1},
		}

		for _, neighbor := range neighbors {
			// Skip if the neighbor is out of bounds or is not an "Empty" cell
			if neighbor.X < 0 || neighbor.Y < 0 || neighbor.X >= len(*grid) || neighbor.Y >= len((*grid)[0]) || (*grid)[neighbor.X][neighbor.Y].Type != "Empty" {
				continue
			}

			newCost := costs[current] + 1
			if cost, ok := costs[neighbor]; !ok || newCost < cost {
				costs[neighbor] = newCost
				priority := newCost + heuristic(neighbor, target)
				heap.Push(pq, &priorityQueueItem{value: neighbor, priority: priority})
				from[neighbor] = current
			}
		}
	}
	// If the target is not reached, return an empty path
	return []cellInfo{}
}


func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}



func whisper(cli *client, targetUsername, message string) {
	targetClient, ok := clients.Load(targetUsername)
	if !ok {
		response := fmt.Sprintf("User '%s' not found.\n", targetUsername)
		cli.conn.Write([]byte(response))
		return
	}

	whisperMessage := map[string]interface{}{
		"type":     "whisper",
		"from":     cli.username,
		"message":  message,
	}
	jsonData, err := json.Marshal(whisperMessage)
	if err != nil {
		cli.conn.Write([]byte("Error encoding JSON.\n"))
		return
	}

	targetClient.(*client).conn.Write(jsonData)
	cli.conn.Write([]byte("Message sent.\n"))
}

func help(cli *client) {
	helpMessages := []map[string]string{
		{"command": "/help", "description": "Show this help message."},
		{"command": "/whisper [username] [message]", "description": "Send a private message to the specified user."},
		{"command": "/list", "description": "List all connected users."},
		{"command": "/mute [username]", "description": "Mute the specified user."},
		{"command": "/unmute [username]", "description": "Unmute the specified user."},
		{"command": "/move [direction]", "description": "Move to an adjacent cell in the specified direction (north, east, south, or west)."},
		{"command": "/travel", "description": "Generate a JWT to travel to another server."},
		{"command": "/map", "description": "Show the current 2D grid map."},
	}

	helpData := map[string]interface{}{
		"type":    "help",
		"commands": helpMessages,
	}
	jsonData, err := json.Marshal(helpData)
	if err != nil {
		cli.conn.Write([]byte("Error encoding JSON.\n"))
		return
	}

	cli.conn.Write(jsonData)
}



// Save the map to a JSON file.
func saveMap(grid [][]*Cell, filename string) error {
	jsonData, err := json.Marshal(grid)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return err
	}

	return nil
}

// Load the map from a JSON file.
func loadMap(filename string) ([][]*Cell, error) {
	jsonFile, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer jsonFile.Close()

	byteValue, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		return nil, err
	}

	var grid [][]*Cell
	err = json.Unmarshal(byteValue, &grid)
	if err != nil {
		return nil, err
	}

	return grid, nil
}

func moveTo(cli *client, targetX, targetY int, sleepDelay time.Duration) {
	start := cellInfo{X: cli.x, Y: cli.y}
	target := cellInfo{X: targetX, Y: targetY}

	path := aStarPathfinding(start, target, &grid)

	if len(path) == 0 {
		cli.conn.Write([]byte("{\"type\":\"move_error\", \"message\":\"Path not found.\"}\n"))
		return
	}

	go func() {
		for _, step := range path {
			time.Sleep(sleepDelay)

			cli.x = step.X
			cli.y = step.Y

			response := fmt.Sprintf("{\"type\":\"move\", \"username\":\"%s\", \"position\":{\"x\":%d, \"y\":%d}}\n", cli.username, cli.x, cli.y)
			announceMove(cli, response)
		}
	}()
}

func announceMove(cli *client, msg string) {
	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		client.conn.Write([]byte(msg))
		return true
	})
}

func sendJSON(conn net.Conn, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		fmt.Println("Error marshaling JSON data:", err)
		return
	}
	conn.Write(append(jsonData, '\n'))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{Status: "OK"}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func loadUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req LoadUserRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	loadedUsers[req.Username] = req
	w.WriteHeader(http.StatusCreated)
}


func startAPI() {
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/api/loadUser", loadUserHandler)
	http.HandleFunc("/api/kickUser", kickUserHandler)
	http.HandleFunc("/api/sendAnnouncement", sendAnnouncementHandler)
	http.HandleFunc("/api/kickAllUsers", kickAllUsersHandler)
	http.HandleFunc("/api/sendMessageToUser", sendMessageToUserHandler)
	http.HandleFunc("/api/moveUser", moveUserHandler)
	http.HandleFunc("/api/sendMessageToCell", sendMessageToCellHandler)
	http.HandleFunc("/muteUser", muteUserHandler)
	http.HandleFunc("/saveMap", saveMapHandler)
	http.HandleFunc("/loadMap", loadMapHandler)

	fmt.Println("Starting API server on :3000")
	http.ListenAndServe(":3000", nil)
}

func kickUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	kicked := false

	// Iterate over the clients sync.Map to find the user and disconnect them
	clients.Range(func(_, v interface{}) bool {
		cli := v.(*client)
		if cli.username == req.Username {
			cli.conn.Close()
			kicked = true
			return false
		}
		return true
	})

	if kicked {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}


func sendAnnouncementHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req struct {
		Message json.RawMessage `json:"message"`
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Broadcast the message to all connected clients
	clients.Range(func(_, v interface{}) bool {
		cli := v.(*client)
		sendJSON(cli.conn, req.Message)
		return true
	})

	w.WriteHeader(http.StatusOK)
}

func kickAllUsersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Iterate over the clients sync.Map and disconnect all users
	clients.Range(func(_, v interface{}) bool {
		cli := v.(*client)
		cli.conn.Close()
		return true
	})

	w.WriteHeader(http.StatusOK)
}

func sendMessageToUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var payload sendMessagePayload
	err := decoder.Decode(&payload)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	toClient, ok := clients.Load(payload.ToUsername)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	toCli := toClient.(*client)
	sendJSON(toCli.conn, map[string]interface{}{
		"type":       "private_message",
		"from":       payload.FromUsername,
		"fromServer": payload.FromServer,
		"message":    payload.Message,
	})

	w.WriteHeader(http.StatusOK)
}

func moveUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rpgAuthHeader := r.Header.Get("RPG_AUTH")
	if rpgAuthHeader != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	decoder := json.NewDecoder(r.Body)
	var payload moveUserPayload
	err := decoder.Decode(&payload)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	cli, ok := clients.Load(payload.Username)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	client := cli.(*client)

	//if !isValidMove(client.x, client.y, payload.X, payload.Y) {
	//	w.WriteHeader(http.StatusBadRequest)
	//	return
	//}

	// Move the user and announce to all connected clients
	moveClient(client, payload.X, payload.Y)

	w.WriteHeader(http.StatusOK)
}

func isValidMove(currentX, currentY, targetX, targetY int) bool {
	// Check if the target coordinates are within the grid boundaries
	if targetX < 0 || targetY < 0 || targetX >= len(grid) || targetY >= len(grid[0]) {
		return false
	}

	// Check if the target cell is not a mountain cell
	if grid[targetX][targetY].Type != Empty {
		return false
	}

	// Check if the movement is a valid adjacent cell (horizontal or vertical only)
	if abs(currentX-targetX)+abs(currentY-targetY) == 1 {
		return true
	}

	return false
}

func sendMessageToCellHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Check if the RPG_AUTH header is valid
	if r.Header.Get("RPG_AUTH") != rpgAuthPassword {
		http.Error(w, "Invalid RPG_AUTH header", http.StatusUnauthorized)
		return
	}

	// Parse JSON payload
	var payload struct {
		X       int    `json:"x"`
		Y       int    `json:"y"`
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Check if coordinates are within the grid bounds
	if payload.X < 0 || payload.Y < 0 || payload.X >= len(grid) || payload.Y >= len(grid[0]) {
		http.Error(w, "Coordinates out of bounds", http.StatusBadRequest)
		return
	}

	// Get the cell at the specified coordinates
	cell := grid[payload.X][payload.Y]

	// Send the message to all clients in the cell
	cell.Clients.Range(func(_, v interface{}) bool {
		cli := v.(*client)
		jsonMessage := map[string]string{"type": "cell_message", "message": payload.Message}
		sendJSON(cli.conn, jsonMessage)
		return true
	})
}

func mute(cli *client, args []string) {
	if len(args) < 2 {
		sendJSON(cli.conn, map[string]string{"type": "error", "message": "Usage: /mute <username>"})
		return
	}

	targetUsername := args[1]

	if _, ok := cli.mutedUsernames[targetUsername]; ok {
		sendJSON(cli.conn, map[string]string{"type": "error", "message": fmt.Sprintf("%s already exists in the muted users", targetUsername)})
	} else {
		cli.mutedUsernames[targetUsername] = true
		sendJSON(cli.conn, map[string]string{"type": "success", "message": fmt.Sprintf("Muted %s", targetUsername)})
	}
}

func unmute(cli *client, args []string) {
	if len(args) < 2 {
		sendJSON(cli.conn, map[string]string{"type": "error", "message": "Usage: /mute <username>"})
		return
	}

	targetUsername := args[1]

	if _, ok := cli.mutedUsernames[targetUsername]; ok {
		delete(cli.mutedUsernames, targetUsername)
		sendJSON(cli.conn, map[string]string{"type": "success", "message": fmt.Sprintf("Unmuted %s", targetUsername)})
	} else {
		sendJSON(cli.conn, map[string]string{"type": "error", "message": fmt.Sprintf("%s is not in the mute list to unmute", targetUsername)})
	}
}

func muteUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("RPG_AUTH") != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Missing username parameter"))
		return
	}

	var userFound bool

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		if client.username == username {
			client.muted = true
			userFound = true
			return false
		}
		return true
	})

	if userFound {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("User muted"))
	} else {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("User not found"))
	}
}

func saveMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("RPG_AUTH") != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	err := saveMap(grid, "map.json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error saving map: %v", err)))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Map saved"))
}

func loadMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("RPG_AUTH") != rpgAuthPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	err := loadMap("map.json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error loading map: %v", err)))
		return
	}

	clients.Range(func(_, v interface{}) bool {
		client := v.(*client)
		announceMap(client)
		return true
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Map loaded and announced to clients"))
}

