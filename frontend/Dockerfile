FROM node:26-trixie-slim AS dev
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY . .
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0", "--port", "80", "--strictPort"]

FROM dev AS build
RUN npm run build

FROM nginx:1.30-alpine AS prod
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
