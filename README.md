# go-home-assignment

**Realtime GPT-4o-mini CLI (Go)**


## Setup
**API key** that was given by Ofir saved as env (read from `OPENAI_API_KEY`)


## Run
```bash
# first time
go mod init realtime-cli

go mod tidy

go run main.go

# or build a binary
go build -o rtcli
./rtcli
```


## Use
- Type a prompt and press **Enter**.
- Type `exit` to quit.


## Examples
```text
Welcome to Real-time GPT-4o-mini CLI with Function Calling!
Type your prompt and press Enter to generate a response or type 'exit' to leave.

You> hi
Chatbot> Hello! How can I assist you today?

You> 6*7
Chatbot> The result of 6 multiplied by 7 is 42.

You> exit
Thanks for using my system, see you next time!
```


## How it works
- Connects to `wss://api.openai.com/v1/realtime?model=gpt-4o-mini-realtime-preview`
- A reader goroutine loops on `conn.Read`, decodes JSON, and sends events on a channel.
- Main goroutine sends requests and consumes events.
- Sends `conversation.item.create` for the user text, then `response.create` to ask the model to answer.
- If the model calls `multiply`, the app buffers args (`response.function_call_arguments.*`), runs local `multiply(a,b)`, sends `conversation.item.create` with `function_call_output`, then triggers another `response.create` so the model finalizes the message.

