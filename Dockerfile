# build

FROM golang:1.18-buster AS build
WORKDIR /app
COPY go.mod go.sum main.go run.go ./
RUN go mod tidy && go build -o /main && echo $PATH

# deploy

FROM gcr.io/distroless/base-debian10
WORKDIR /
COPY --from=build /usr/local/go /usr/local/go
COPY --from=build /main         /main
ENV PATH="$PATH":/usr/local/go/bin
USER nonroot:nonroot
ENTRYPOINT ["/main"]
