# Pastebox
curl 기반 파일 공유 서비스

[English](./README.md) | Korean

![](./preview.png)

### 기술 스택
| 레이어 | 스택 |
|--------|------|
| OS | Alpine Linux 3.23.4 (미러: https://mirror5.krfoss.org/alpine)|
| 언어 | Go |
| 프론트엔드 | Go HTML Template |
| 백엔드 | Go Standard Library HTTP Server |
| 저장소 | Local File Storage |

### 어떻게 사용하나요?
1. 저장소를 클론하거나 `.zip` 파일로 다운로드하세요.
2. docker compose를 사용하여 빌드 후 실행하세요: `docker compose up -d --build`
3. `http://localhost:3000`를 브라우저에서 접속하거나 NGINX, Caddy, Traefik을 통해 리버스프록시를 구축하여 도메인으로 접속하세요.
4. `curl`을 사용하여 파일을 업로드하세요.

### 기능

1. **파일 자동삭제**: 업로드 시점 기준 30일 후 자동삭제

2. **텍스트 업로드**: **echo, cat (cat << EOF)**와 같이 리눅스 명령어와 연계하여 업로드 가능
   ```
   echo "hello" | curl -X POST --data-binary @- http://localhost:8080/
   ```
   
3. **파일 업로드**: `multipart/form-data` 형식의 파일 업로드 지원
   ```
   curl -F "file=@test.txt" http://localhost:8080/
   ```

4. **영구 저장**: `data-policy: permanent` 헤더를 사용한 영구 저장 지원

5. **비밀번호 링크**: `usepassword: true` 헤더를 사용한 비공개 업로드 링크생성 지원 (헤더 사용시 **영문(대+소문자) + 숫자 + 특수문자** 조합으로 생성된 8자리 비밀번호 발급 및 `?password=...` 혹은 `paste-password: ...` 헤더로 접근 가능)
   ```
   # 비밀번호 링크 생성:
   curl -H "usepassword: true" -F "file=@secret.txt" http://localhost:8080/
   
   # 파일 확인:
   curl -H "paste-password: RANDOM_PASSWORD" http://localhost:8080/RANDOM_CODE

   또는

   curl http://localhost:8080/RANDOM_CODE?password=RANDOM_PASSWORD
   ```

   ![](./preview2.png)
   ![](./preview3.png)

### 디렉토리 구조
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
