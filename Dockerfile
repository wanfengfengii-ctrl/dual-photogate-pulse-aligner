# 单一镜像同时承载长期运行的 api 服务与一次性 verify 验收测试：
# verify 需要 Go 工具链运行 go test，故以官方 Go 1.24 为基础镜像。
FROM golang:1.24-bookworm

WORKDIR /src

# 先拷贝依赖清单以利用构建缓存。
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并预编译：api 二进制与验收测试二进制，运行期无需联网、无需再编译。
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/server ./cmd/server \
    && CGO_ENABLED=0 go test -c -o /usr/local/bin/acceptance.test ./test

ENV API_PORT=8080
EXPOSE 8080

CMD ["/usr/local/bin/server"]
