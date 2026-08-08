FROM golang:1.25

# Set destination for COPY
WORKDIR /app

# Copy the whole module (go.mod uses a local "replace" pointing at the
# repository root, so the module source needs to be in the build context).
COPY . .

WORKDIR /app/examples/simpleDemo

# Download Go modules
RUN go mod download

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o /simpleDemo .

# Optional:
# To bind to a TCP port, runtime parameters must be supplied to the docker command.
# But we can document in the Dockerfile what ports
# the application is going to listen on by default.
# https://docs.docker.com/engine/reference/builder/#expose
EXPOSE 8080

# Run
CMD ["/simpleDemo"]
