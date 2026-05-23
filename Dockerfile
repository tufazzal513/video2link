FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.21 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY . .

# লাইব্রেরিগুলোর মিসিং হ্যাশ এবং ডিপেনডেন্সি অটো-সিঙ্ক করার জন্য
RUN go mod tidy

# গো প্রজেক্ট কম্পাইল এবং বিল্ড করার জন্য
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /app/fsb -ldflags="-w -s" ./cmd/fsb

# ফাইনাল রানটাইম কন্টেইনার হিসেবে alpine ব্যবহার
FROM alpine:latest

# ফায়ারবেস ও টেলিগ্রামের সিকিউরড কানেকশনের জন্য সার্টিফিকেট এবং টাইমজোন যুক্ত করা
RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /app/fsb /app/fsb
ENTRYPOINT ["/app/fsb"]
