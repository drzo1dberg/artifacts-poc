FROM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json tsconfig.json ./
RUN npm ci
COPY src ./src
RUN npm run build

FROM nginx:1.29-alpine
COPY index.html style.css /usr/share/nginx/html/
COPY --from=build /src/dist/ /usr/share/nginx/html/
