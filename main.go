package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/mattn/go-colorable"
	"github.com/sirupsen/logrus"
	"nhooyr.io/websocket"

	kvclient "github.com/strimertul/kilovolt-client-go/v6"
)

type ClientCredentialsResult struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
	CreatedAt    string  `json:"created_at"`
	Expires      int     `json:"expires_in"`
	Scope        string  `json:"scope"`
	TokenType    string  `json:"token_type"`
}

type ChatMessage struct {
	Message string `json:"message"`
	User    struct {
		Username string `json:"username"`
	} `json:"user"`
}

type ChatMessageResult struct {
	Result struct {
		Data struct {
			ChatMessage ChatMessage `json:"chatMessage"`
		} `json:"data"`
	} `json:"result"`
}

type GQLQuery struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

func check(err error, format string, args ...interface{}) {
	if err != nil {
		args = append(args, err)
		_, _ = fmt.Fprintf(os.Stderr, format+": %s\n", args...)
		os.Exit(1)
	}
}

func parseLogLevel(level string) logrus.Level {
	switch level {
	case "error":
		return logrus.ErrorLevel
	case "warn", "warning":
		return logrus.WarnLevel
	case "info", "notice":
		return logrus.InfoLevel
	case "debug":
		return logrus.DebugLevel
	case "trace":
		return logrus.TraceLevel
	default:
		return logrus.InfoLevel
	}
}

func main() {
	endpoint := flag.String("kv-endpoint", "http://localhost:4337/ws", "Kilovolt endpoint")
	password := flag.String("password", "", "Optional password for Kilovolt")
	prefix := flag.String("prefix", "glimesh/", "Prefix/Namespace for keys")
	channelID := flag.Int("channel-id", -1, "Glimesh channel ID")
	clientID := flag.String("client-id", "", "Glimesh app client ID")
	clientSecret := flag.String("client-secret", "", "Glimesh app secret key")
	chatHistorySize := flag.Int("chat-history", 6, "Number of chat messages to keep in history")
	loglevel := flag.String("log-level", "info", "Logging level (debug, info, warn, error)")
	flag.Parse()

	log := logrus.New()
	log.SetLevel(parseLogLevel(*loglevel))
	ctx := context.Background()

	// Ok this is dumb but listen, I like colors.
	if runtime.GOOS == "windows" {
		log.SetFormatter(&logrus.TextFormatter{ForceColors: true})
		log.SetOutput(colorable.NewColorableStdout())
	}

	if *clientID == "" || *clientSecret == "" {
		log.Fatal("You must provide a client ID and secret key, check https://glimesh.tv/users/settings/applications/new to make a new Glimesh.tv application")
	}

	if *channelID == -1 {
		log.Fatal("You must provide a channel ID")
	}

	// Connect to strimertul/Kilovolt
	client, err := kvclient.NewClient(*endpoint, kvclient.ClientOptions{Password: *password})
	check(err, "Connection to kilovolt failed")
	defer client.Close()

	chatEventKey := fmt.Sprintf("%sev/chat-message", *prefix)
	chatRPCKey := fmt.Sprintf("%s@send-chat-message", *prefix)
	chatHistoryKey := fmt.Sprintf("%schat-history", *prefix)
	var chatHistory []ChatMessage
	// Get old chat history, if available
	err = client.GetJSON(chatHistoryKey, &chatHistory)
	if err != nil {
		chatHistory = make([]ChatMessage, 0)
		_ = client.SetJSON(chatHistoryKey, chatHistory)
	}

	// Obtain a token from Glimesh OAuth
	res, err := http.Post("https://glimesh.tv/api/oauth/token", "application/x-www-form-urlencoded",
		strings.NewReader(fmt.Sprintf("grant_type=client_credentials&client_id=%s&client_secret=%s&scope=chat", *clientID, *clientSecret)))
	check(err, "Could not retrieve Glimesh API token")

	credentials := ClientCredentialsResult{}
	err = jsoniter.ConfigFastest.NewDecoder(res.Body).Decode(&credentials)
	check(err, "Could not decode Glimesh API response")

	// Connect to Glimesh
	c, _, err := websocket.Dial(ctx, fmt.Sprintf("wss://glimesh.tv/api/socket/websocket?vsn=2.0.0&token=%s", credentials.AccessToken), nil)
	check(err, "Could not connect to Glimesh websocket")
	defer c.Close(websocket.StatusGoingAway, "app was closed")

	check(c.Write(ctx, websocket.MessageText, []byte("[\"1\",\"1\",\"__absinthe__:control\",\"phx_join\",{}]")), "Could not send join message")
	check(c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf("[\"1\",\"2\",\"__absinthe__:control\",\"doc\",{\"query\":\"subscription{ chatMessage(channelId: %d) { user { username } message } }\",\"variables\":{} }]", *channelID))), "Could not send join message")

	log.WithField("endpoint", *endpoint).Info("Connected to Kilovolt")

	wsmsg := make(chan ChatMessage)
	go func() {
		for {
			mtyp, byt, err := c.Read(ctx)
			if err != nil {
				if err != io.EOF {
					log.WithError(err).Fatal("Could not read from websocket")
				}
				log.WithError(err).Fatal("Connection was closed by remote")
			}
			log.Debug(string(byt))
			if mtyp == websocket.MessageText {
				var msgType *string
				var msgSubType *string
				var subId string
				var subType string
				var result ChatMessageResult

				payload := []interface{}{&msgType, &msgSubType, &subId, &subType, &result}
				err := jsoniter.ConfigFastest.Unmarshal(byt, &payload)
				if err != nil {
					log.WithError(err).Error("Could not decode websocket message")
					continue
				}

				if msgType == nil && msgSubType == nil {
					wsmsg <- result.Result.Data.ChatMessage
				}
			}
		}
	}()

	incoming, err := client.SubscribeKey(chatRPCKey)
	if err != nil {
		log.WithError(err).Fatal("Could not subscribe to chat RPC key")
	}

	// Create a 30 sec ticker for heartbeats to Glimesh
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ticker.C:
			check(c.Write(ctx, websocket.MessageText, []byte("[\"1\",\"3\",\"phoenix\",\"heartbeat\",{}]")), "Could not send heartbeat")
		case msg := <-wsmsg:
			log.WithField("user", msg.User.Username).Debug("Received message")
			err := client.SetJSON(chatEventKey, msg)
			if err != nil {
				log.WithField("key", chatEventKey).WithError(err).Error("Could not set chat key")
			}
			chatHistory = append(chatHistory, msg)
			if len(chatHistory) > *chatHistorySize {
				chatHistory = chatHistory[len(chatHistory)-*chatHistorySize:]
			}
			err = client.SetJSON(chatHistoryKey, chatHistory)
			if err != nil {
				log.WithField("key", chatHistoryKey).WithError(err).Error("Could not set chat key")
			}
		case kv := <-incoming:
			log.WithField("key", kv.Key).Debug("Received RPC message")
			// Escape and clean message
			message := strings.TrimSpace(strings.Replace(kv.Value, "\"", "\\\"", -1))
			// Prepare payload
			payload := fmt.Sprintf(`mutation {createChatMessage(channelId: %d, message: {message: "%s"}) { message }}`, *channelID, message)
			byt, err := jsoniter.ConfigFastest.Marshal([]interface{}{"1", "4", "__absinthe__:control", "doc", GQLQuery{Query: payload, Variables: map[string]interface{}{}}})
			if err != nil {
				log.WithError(err).Error("Could not encode chat message")
				continue
			}
			fmt.Printf("%s\n", string(byt))
			err = c.Write(ctx, websocket.MessageText, byt)
			if err != nil {
				log.WithError(err).Error("Could not send chat message")
				continue
			}
			log.Debug("Sent message")
		}
	}
}
