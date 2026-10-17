ARG golang_version
FROM golang:${golang_version} AS build
ARG build_version=dev
ARG build_commit=none
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${build_version} -X main.commit=${build_commit}" \
    -o /manager ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
