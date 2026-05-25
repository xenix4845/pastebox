# Pastebox
curl-based file sharing service

English | [Korean](./README_ko.md)

![](./preview.png)

### Tech stack
| Layer | Stack |
|--------|------|
| OS | Alpine Linux 3.23.4 (mirror: https://mirror5.krfoss.org/alpine) |
| Language | Go |
| Frontend | Go HTML Template |
| Backend | Go Standard Library HTTP Server |
| Storage | Local File Storage |

*If there is a specific mirror you want to use, you can modify it in the Dockerfile.*

### How to use?
1. Clone the repository or download it as a .zip file.
2. Build and run using docker compose: `docker compose up -d --build`.
3. Open `http://localhost:3000` in your browser, or access the service through a reverse proxy configured with Nginx, Traefik, or Caddy.
4. Upload a file using `curl`.

### Features
> [!NOTE]
> **DON'T FORGET TO REPLACE `localhost` WITH THE DOMAIN OR IP ADDRESS YOU'RE CURRENTLY USING.**

1. **Automatic File Deletion**: Files are automatically deleted 30 days after upload.

2. **Text Upload**: Supports uploading text directly from Linux commands such as **echo** and **cat (cat << EOF)**.
   ```
   echo "hello" | curl -X POST --data-binary @- http://localhost:8080/
   ```

3. **File Upload**: Supports file uploads using the `multipart/form-data` format.
   ```
   curl -F "file=@test.txt" http://localhost:8080/
   ```

4. **Permanent Storage**: Supports permanent file storage using the `data-policy: permanent` header.

5. **Password-Protected Links**: Supports private upload links using the `usepassword: true` header.

   When enabled, an 8-character password containing uppercase/lowercase letters, numbers, and special characters is automatically generated. Files can be accessed using either the `?password=...` query parameter or the `paste-password: ...` header.
   ```
   # Create password-protected link:
   curl -H "usepassword: true" -F "file=@secret.txt" http://localhost:8080/
   
   # View file:
   curl -H "paste-password: RANDOM_PASSWORD" http://localhost:8080/RANDOM_CODE

   or
   
   curl http://localhost:8080/RANDOM_CODE?password=RANDOM_PASSWORD
   ```
   
   ![](./preview2.png)
   ![](./preview3.png)

### Directory structure
```text
pastebox/
├── Dockerfile
├── docker-compose.yml
├── docker-entrypoint.sh
├── go.mod
├── README.md
├── README_ko.md
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── metadata.go
│   └── store.go
└── templates/
    └── index.html
```
