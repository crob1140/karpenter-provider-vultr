FROM golang:1.26.3 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/karpenter-provider-vultr ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/karpenter-provider-vultr /karpenter-provider-vultr
USER nonroot:nonroot
ENTRYPOINT ["/karpenter-provider-vultr"]
