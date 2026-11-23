package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName    = "github.com/zraisan/warpspan"
	instrumentationVersion = "0.1.0"
)

var tracer = otel.GetTracerProvider().Tracer(
	instrumentationName,
	trace.WithInstrumentationVersion(instrumentationVersion),
)

func handleRequest(ctx context.Context) error {
	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://google.com", nil)

	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	resp, err := client.Do(req)

	if err != nil {
		return fmt.Errorf(err.Error())
	}
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf(err.Error())
	}

	return nil

}

func main() {
	ctx := context.Background()

	exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		log.Fatalf("creating stdout exporter: %v", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
	)
	defer tracerProvider.Shutdown(ctx)

	otel.SetTracerProvider(tracerProvider)

	ctx, span := tracer.Start(ctx, "GET google.com", trace.WithSpanKind(1))
	defer span.End()

	err = handleRequest(ctx)
	if err != nil {
		log.Fatalf("Error")
	}

}
