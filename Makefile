
build:
	docker build -f Dockerfile.dev --target build --tag build .
	docker build -f Dockerfile.dev --target cdp --tag cdp .

run:
	docker-compose up

infra:
	docker run --name cirri -it \
		-p 80:80 \
		-p 443:443 \
		-e GANDIV5_API_KEY \
		-e STACKDOMAIN \
		--env-file .env \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v image_caddy_data:/data \
		--net cirri_proxy \
			cdp


list:
	docker run --rm -it cdp caddy list-modules