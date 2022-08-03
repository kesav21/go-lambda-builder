# run: ../backend-api-functions/lambdas/builder
# 	@cd ../backend-api-functions/lambdas && ./builder

../backend-api-functions/lambdas/builder: builder
	@cp $^ $@

builder: go.mod go.sum main.go run.go
	@go build

image: Dockerfile go.mod go.sum main.go run.go
	@docker build -t go-lambda-builder:latest .

clean:
	@rm -f builder ../backend-api-functions/lambdas/builder

edit:
	@nvim Makefile Dockerfile main.go run.go

.PHONY: image clean run edit
