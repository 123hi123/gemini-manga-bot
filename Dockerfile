# 建置階段
FROM golang:1.22-alpine AS builder

WORKDIR /app

# 複製 go.mod
COPY go.mod ./

# 複製原始碼
COPY . .

# 下載依賴並建置（純 Go，不需要 CGO）
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-s -w' -o gemini-manga-bot .

# 執行階段
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# 從建置階段複製執行檔
COPY --from=builder /app/gemini-manga-bot .

# 建立資料目錄
RUN mkdir -p /app/data

# 環境變數
ENV GEMINI_API_KEY=""
ENV BOT_TOKEN=""
ENV DATA_DIR="/app/data"

# 執行
CMD ["./gemini-manga-bot"]
