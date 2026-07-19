# gawk-app frontend image: Vite build served by unprivileged nginx.
# Build context is gawk-app/:
#
#   docker build -f deploy/Dockerfile -t gawk-app:dev .

FROM node:26-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# nginx-unprivileged listens on 8080 and runs as a non-root user out of the
# box — no securityContext fights in k8s.
FROM nginxinc/nginx-unprivileged:1.31-alpine
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 8080
