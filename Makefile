.PHONY: build test vet run clean frontend

# 单二进制（纯 Go SQLite 驱动，无 CGO）。改过前端需先 make frontend。
build:
	go build -o bin/mailhub.exe ./cmd/mailhub

# 前端：构建产物 web/dist 由 web/embed.go 内嵌（Node ≥ 18）
frontend:
	cd web && npm install && npm run build

# 完整构建：前端 + Go（首次或前端变更后用这个）
all: frontend build

test:
	go test ./...

vet:
	go vet ./...

# 前端开发热重载（:5173，/api 代理到 :18099）
dev:
	cd web && npm run dev

# 本地运行（需先设置 MAILHUB_API_TOKEN 与 MAILHUB_SECRET_KEY，见 README/DESIGN.md 9.1）
run:
	go run ./cmd/mailhub

gen-secret:
	go run ./cmd/mailhub --gen-secret

clean:
	rm -rf bin web/dist/assets
