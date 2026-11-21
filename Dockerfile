FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

# pandoc/core ships pandoc 3.x on Alpine; ffmpeg/ffprobe come from Alpine packages.
FROM pandoc/core:3.5
RUN apk add --no-cache ffmpeg tzdata ca-certificates
WORKDIR /app
COPY --from=build /out/server /app/server
COPY deploy/reference.docx /app/deploy/reference.docx
ENV TZ=Asia/Jakarta DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/app/server"]
