##
## Build Stage
##

FROM        golang:latest as build

ENV         GOOS=linux
ENV         GOARCH=amd64
ENV         GO111MODULE=on
ENV         CGO_ENABLED=0

WORKDIR     /workspace

COPY        . . 

RUN         go get ./... && \
            go build -o bin ./cmd/main.go

##
## Final Stage
##

FROM        scratch as final

ENV         GIN_MODE=release
 
COPY        --from=build /workspace/bin ./

ENTRYPOINT ["/bin"]
