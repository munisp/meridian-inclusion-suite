FROM node:20-bookworm-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
# Perf: gzip JS/CSS/SVG (nginx default gzips text/html only — the ~310 kB
# bundle shipped uncompressed), and mark content-hashed /assets/ immutable so
# repeat visits never revalidate; index.html stays no-cache.
RUN printf 'server {\n  listen 8080;\n  root /usr/share/nginx/html;\n  gzip on;\n  gzip_types text/css application/javascript image/svg+xml application/json;\n  gzip_min_length 1024;\n  location /assets/ {\n    add_header Cache-Control "public, max-age=31536000, immutable";\n    try_files $uri =404;\n  }\n  location / {\n    add_header Cache-Control "no-cache";\n    try_files $uri $uri/ /index.html;\n  }\n}\n' > /etc/nginx/conf.d/default.conf
EXPOSE 8080
