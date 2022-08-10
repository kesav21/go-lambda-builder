package main

import (
	"context"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

func main() {
	lambda.Start(func(
		ctx context.Context,
		request events.APIGatewayV2HTTPRequest,
	) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Body:       "hello from testLambda08",
		}, nil
	})
}
