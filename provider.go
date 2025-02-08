package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	genai "github.com/google/generative-ai-go/genai"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/api/option"
)

type openaiProvider struct {
	client *openai.Client
	model  string
}

func (p *openaiProvider) StreamChat(ctx context.Context, messages []Message) (io.ReadCloser, error) {
	chatMessages := make([]openai.ChatCompletionMessage, len(messages))
	for i, msg := range messages {
		chatMessages[i] = openai.ChatCompletionMessage{
			Role:    msg.Role,
			Content: msg.Content,
		}
	}

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: chatMessages,
		Stream:   true,
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("openaiProvider.StreamChat: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			response, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Write([]byte(response.Choices[0].Delta.Content))
		}
	}()

	return pr, nil
}

func (p *openaiProvider) Close() error {
	return nil // not necessary
}

type googleProvider struct {
	client *genai.Client
	model  *genai.GenerativeModel
	chat   *genai.ChatSession
}

func (p *googleProvider) StreamChat(ctx context.Context, messages []Message) (io.ReadCloser, error) {
	if p.chat == nil {
		p.chat = p.model.StartChat()
	}

	p.chat.History = make([]*genai.Content, 0, len(messages)-1)
	for _, msg := range messages[:len(messages)-1] {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		p.chat.History = append(p.chat.History, &genai.Content{
			Role:  role,
			Parts: []genai.Part{genai.Text(msg.Content)},
		})
	}

	resp := p.chat.SendMessageStream(ctx, genai.Text(messages[len(messages)-1].Content))
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			response, err := resp.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if len(response.Candidates) > 0 && len(response.Candidates[0].Content.Parts) > 0 {
				if text, ok := response.Candidates[0].Content.Parts[0].(genai.Text); ok {
					pw.Write([]byte(string(text)))
				}
			}
		}
	}()

	return pr, nil
}

func (p *googleProvider) Close() error {
	return nil // not necessary
}

func NewProvider(conf config) (Provider, error) {
	switch conf.Provider {
	case "openai":
		return &openaiProvider{
			client: openai.NewClient(conf.Key),
			model:  conf.Model,
		}, nil
	case "google":
		client, err := genai.NewClient(ctx, option.WithAPIKey(conf.Key))
		if err != nil {
			return nil, fmt.Errorf("NewProvider/google: %w", err)
		}
		return &googleProvider{
			client: client,
			model:  client.GenerativeModel(conf.Model),
		}, nil
	default:
		return nil, fmt.Errorf("NewProvider: Unknown provider: %s", conf.Provider)
	}
}
