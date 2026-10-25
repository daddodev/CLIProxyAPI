FROM oven/bun:1.3.14 AS management-builder

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /management

# Pin the panel source so every image contains the matching quota controls.
ARG MANAGEMENT_UI_COMMIT=601bfc7d84e7c17ed8fd19b6dedb20d92c2b5494
RUN git init && git remote add origin https://github.com/daddodev/Cli-Proxy-API-Management-Center.git && git fetch --depth 1 origin ${MANAGEMENT_UI_COMMIT} && git checkout --detach FETCH_HEAD
RUN bun install --frozen-lockfile && bun run build

FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

COPY --from=management-builder /management/dist/index.html /CLIProxyAPI/static/management.html

ENV MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
