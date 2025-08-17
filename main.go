package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const (
	realtimeURL          = "wss://api.openai.com/v1/realtime"
	modelName            = "gpt-4o-mini-realtime-preview"
	defaultInstructions  = "Provide a detailed response."
	multipleInstractions = "When the user asks to multiply two numbers call the multiply tool."
)

// -------------------------- initializition --------------------------

func loadAPIKey() (string, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", errors.New("OPENAI_API_KEY not found")
	}
	return apiKey, nil
}

// -------------------------- DIAL --------------------------

func dialRealtime(ctx context.Context, apiKey, model string) (*websocket.Conn, error) {
	url := fmt.Sprint(realtimeURL, "?model=", model)
	header := http.Header{
		"Authorization": []string{"Bearer " + apiKey},
		"OpenAI-Beta":   []string{"realtime=v1"},
	}

	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("websocket handshake failed: %s: %w", resp.Status, err)
		}
		return nil, err
	}
	return conn, nil
}

// -------------------------- WRITE --------------------------

// this function adds a new conversation item to the time line (but it doesnt mean that the model will start generating a response yet)
func sendUserInput(ctx context.Context, c *websocket.Conn, textInput string) error {
	conversationItemObj := map[string]any{
		"type": "conversation.item.create", //for realtime conversation
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": textInput,
				},
			},
		},
	}
	return marshalAndSend(ctx, c, conversationItemObj)
}

// this function will ask to actually generate a response (using the instructions too)
func requestTextResponse(ctx context.Context, c *websocket.Conn, instructions string) error {
	responseRequestObj := map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities":   []string{"text"}, //make sure the response will be in a text format
			"instructions": instructions,
		},
	}
	return marshalAndSend(ctx, c, responseRequestObj)
}

// -------------------------- TOOL --------------------------
func addMultipleToTools(ctx context.Context, c *websocket.Conn) error {
	body := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": defaultInstructions + multipleInstractions,
			"tools": []map[string]any{
				{
					"type":        "function",
					"name":        "multiply",
					"description": "Multiply two numbers and return the result.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"a": map[string]any{"type": "number"},
							"b": map[string]any{"type": "number"},
						},
						"required": []string{"a", "b"},
					},
				},
			},
		},
	}
	return marshalAndSend(ctx, c, body)
}

func sendFunctionOutput(ctx context.Context, c *websocket.Conn, callID string, outputJSON string) error {
	msg := map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  outputJSON,
		},
	}
	return marshalAndSend(ctx, c, msg)
}

func marshalAndSend(ctx context.Context, c *websocket.Conn, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err) //conversion error
	}
	err = c.Write(ctx, websocket.MessageText, jsonData)
	if err != nil {
		return fmt.Errorf("write error: %w", err) //error to write it to the web socket
	}
	return nil
}

// -------------------------- READ and utilities --------------------------

func startReader(ctx context.Context, c *websocket.Conn) (eventsCh <-chan map[string]any, errsCh <-chan error) {
	events := make(chan map[string]any, 128)
	errs := make(chan error, 128)

	go func() {
		defer close(events)
		defer close(errs)
		for {
			// make sure that the context wasnt canceled
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			default:
			}

			_, data, err := c.Read(ctx)
			if err != nil {
				errs <- err
				return
			}

			var evt map[string]any
			err = json.Unmarshal(data, &evt)
			if err != nil {
				errs <- fmt.Errorf("reader json unmarshal failed: %w", err)
				continue
			}
			events <- evt
		}
	}()

	return events, errs
}

func waitForEventTypeFromChan(ctx context.Context, events <-chan map[string]any, expectedEventType string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for %s: %w", expectedEventType, ctx.Err())
		case evt, ok := <-events:
			if !ok {
				return fmt.Errorf("events channel closed while waiting for %s", expectedEventType)
			}
			typ, _ := evt["type"].(string)
			if typ == "error" {
				b, _ := json.Marshal(evt)
				return fmt.Errorf("server error: %s", string(b))
			}
			if typ == expectedEventType {
				return nil
			}
		}
	}
}

func streamAssistantTextFromChan(ctx context.Context, c *websocket.Conn, events <-chan map[string]any) (string, bool, error) {
	var full string
	needFollowUp, printedWithNoTool := false, false

	argBuf := map[string]*strings.Builder{}

	for {
		select {
		case <-ctx.Done():
			return full, needFollowUp, fmt.Errorf("stream timeout: %w", ctx.Err())

		case evt, ok := <-events:
			if !ok {
				return full, needFollowUp, fmt.Errorf("events channel closed during stream")
			}

			typ, _ := evt["type"].(string)
			if typ == "error" {
				b, _ := json.Marshal(evt)
				return full, needFollowUp, fmt.Errorf("server error: %s", string(b))
			}

			switch typ {
			case "response.text.delta": //not a tool just a normal response
				if d, ok := evt["delta"].(string); ok {
					if !printedWithNoTool {
						fmt.Print("Chatbot> ")
						printedWithNoTool = true
					}
					fmt.Print(d)
					full += d
				}

			case "response.function_call_arguments.delta": //tool response that need to be saved in argBuf for later
				callID, _ := evt["call_id"].(string)
				delta, _ := evt["delta"].(string)
				if callID == "" || delta == "" {
					continue
				}
				buf := argBuf[callID]
				if buf == nil {
					buf = &strings.Builder{}
					argBuf[callID] = buf
				}
				buf.WriteString(delta)

			case "response.function_call_arguments.done": //tool response done now we need to send the argBuf
				callID, _ := evt["call_id"].(string)
				argsJSON, _ := evt["arguments"].(string)
				if argsJSON == "" {
					if b := argBuf[callID]; b != nil {
						argsJSON = b.String()
					}
				}

				var args struct {
					A float64 `json:"a"`
					B float64 `json:"b"`
				}

				err := json.Unmarshal([]byte(argsJSON), &args)
				if err != nil {
					return full, needFollowUp, fmt.Errorf("bad function args: %w", err)
				}

				result := multiply(args.A, args.B)
				out := fmt.Sprintf(`{"result": %g}`, result)
				err = sendFunctionOutput(ctx, c, callID, out)
				if err != nil {
					return full, needFollowUp, err
				}

				delete(argBuf, callID)
				needFollowUp = true //tells the caller to open a new response after this one ends

			case "response.text.done", "response.done":
				if printedWithNoTool {
					fmt.Println()
				}

				return full, needFollowUp, nil
			}
		}
	}
}

// -------------------------- helpers --------------------------
func multiply(a, b float64) float64 { return a * b }

// -------------------------- main --------------------------
func main() {
	apiKey, err := loadAPIKey()
	if err != nil {
		log.Fatal(err)
	}

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDial()
	conn, err := dialRealtime(dialCtx, apiKey, modelName)
	if err != nil {
		log.Fatalf("dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// start a single reader goroutine for the whole session
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()
	eventsCh, errsCh := startReader(sessionCtx, conn)

	// register the multiple function tool
	updCtx, cancelUpd := context.WithTimeout(context.Background(), 10*time.Second)
	if err = addMultipleToTools(updCtx, conn); err != nil {
		cancelUpd()
		log.Fatalf("failed to register tools: %v", err)
	}
	cancelUpd()

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Welcome to Real-time GPT-4o-mini CLI with Function Calling!")
	fmt.Println("Type your prompt and press Enter to generate a response or type 'exit' to leave.\n")

	for {
		// get the input from the user (and exit the program if he ask for it)
		fmt.Print("You> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalf("failed to read the input: %v", err)
		}
		input = strings.TrimSpace(input)
		if strings.EqualFold(input, "exit") {
			fmt.Println("Thanks for using my system, see you next time!")
			return
		}

		// send the user input to create a new conversation item
		sendCtx, cancelSend := context.WithTimeout(context.Background(), 30*time.Second)
		if err = sendUserInput(sendCtx, conn, input); err != nil {
			cancelSend()
			log.Fatalf("failed to send user input: %v", err)
		}
		cancelSend()

		// make sure that the conversation item was created
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Second)
		if err = waitForEventTypeFromChan(waitCtx, eventsCh, "conversation.item.created"); err != nil {
			cancelWait()
			log.Fatal(err)
		}
		cancelWait()

		// generate the response
		reqCtx, cancelReq := context.WithTimeout(context.Background(), 30*time.Second)
		if err = requestTextResponse(reqCtx, conn, defaultInstructions); err != nil {
			cancelReq()
			log.Fatal(err)
		}
		cancelReq()

		// stream the response
		streamCtx, cancelStream := context.WithTimeout(context.Background(), 30*time.Second)
		_, needFollowUp, err := streamAssistantTextFromChan(streamCtx, conn, eventsCh)
		if err != nil {
			cancelStream()
			log.Fatal(err)
		}
		cancelStream()

		if needFollowUp {
			toolResReqCtx, cancelToolResReq := context.WithTimeout(context.Background(), 30*time.Second)
			if err = requestTextResponse(toolResReqCtx, conn, defaultInstructions); err != nil {
				cancelToolResReq()
				log.Fatal(err)
			}
			cancelToolResReq()

			toolResStreamCtx, cancelToolResStream := context.WithTimeout(context.Background(), 30*time.Second)
			_, _, err = streamAssistantTextFromChan(toolResStreamCtx, conn, eventsCh)
			if err != nil {
				cancelToolResStream()
				log.Fatal(err)
			}
			cancelToolResStream()
		}
		fmt.Println()

		// takes care of any reader errors to continue to the iteration if there is no errors
		select {
		case err = <-errsCh:
			if err != nil {
				log.Fatalf("reader error: %v", err)
			}
		default:
		}
	}
}
