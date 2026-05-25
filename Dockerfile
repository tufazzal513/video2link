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

WORKDIR /app
COPY --from=builder /app/fsb /app/fsb

# ফায়ারবেস সিক্রেট ফাইলটি ফাইনাল কন্টেইনারে কপি করা
COPY firebase-adminsdk.json /app/firebase-adminsdk.json

# হাগিং ফেসের সিকিউরিটি (User 1000) রাইট পারমিশনের জন্য
RUN chmod -R 777 /app

# বটের রান (run) কম্যান্ডটি স্বয়ংক্রিয়ভাবে এক্সিকিউট করার জন্য
ENTRYPOINT ["/app/fsb", "run"]
