# Pastebox
curl 기반 파일 공유 서비스

[English](./README.md) | Korean

![](./preview.png)

### 기술 스택
| 레이어 | 스택 |
|--------|------|
| OS | Alpine Linux 3.23.4 (미러: https://mirror5.krfoss.org/alpine) |
| 언어 | Go |
| 프론트엔드 | Go HTML 템플릿 |
| 백엔드 | Go 표준 라이브러리 기반 HTTP 서버 |
| 저장소 | 로컬 파일 저장소 |
| 컨테이너 | Docker + Docker Compose |

*사용하고 싶은 Alpine 미러가 따로 있다면 Dockerfile에서 수정할 수 있습니다.*

### 어떻게 사용하나요?
1. 저장소를 클론하거나 `.zip` 파일로 다운로드하세요.
2. docker compose를 사용하여 빌드 후 실행하세요: `docker compose up -d --build`
3. `http://localhost:3000`를 브라우저에서 접속하거나 NGINX, Caddy, Traefik을 통해 리버스 프록시를 구축하여 도메인으로 접속하세요.
4. `curl`을 사용하여 텍스트 또는 파일을 업로드하세요.

### 기능

> [!NOTE]
> **현재 사용 중인 도메인 또는 IP 주소에 맞게 `localhost`를 반드시 변경하세요.**

1. **파일 자동 삭제**: 업로드 시점 기준 30일 후 자동 삭제됩니다.

2. **텍스트 업로드**: **echo, cat (cat << EOF)**와 같은 Linux 명령어와 연계하여 텍스트를 직접 업로드할 수 있습니다.
   ```
   echo "hello" | curl -X POST --data-binary @- http://localhost:8080/
   ```

3. **파일 업로드**: `multipart/form-data` 형식의 파일 업로드를 지원합니다.
   ```
   curl -F "file=@test.txt" http://localhost:8080/
   ```

4. **영구 저장**: `data-policy: permanent` 헤더를 사용하면 자동 삭제 대상에서 제외되어 영구 저장됩니다.
   ```
   curl -H "data-policy: permanent" -F "file=@test.txt" http://localhost:8080/
   ```
   ```json
   // 저장 경로: ./data/코드.json

   {
     "id": "code",
     "created_at": "2026-05-25T06:46:51.108540924Z",
     "expires_at": "0001-01-01T00:00:00Z",
     "data_policy": "permanent",
     "size": 5,
     "content_type": "application/octet-stream"
   }
   ```

5. **만료 시간 표시**: 일반 업로드는 응답에 `expires` 항목이 포함되어 만료 시간을 확인할 수 있습니다. `data-policy: permanent`를 사용한 경우에는 만료 시간이 표시되지 않습니다.
   ```
   url: http://localhost:8080/RANDOM_CODE
   expires: 2026-06-24T05:10:26Z
   delete: http://localhost:8080/RANDOM_CODE?delete=DELETE_TOKEN
   ```

6. **수동 삭제**: 업로드 시 발급되는 삭제 URL을 사용하여 파일을 직접 삭제할 수 있습니다. 삭제 요청은 컨테이너 로그에도 기록됩니다.
   ```
   # 파일 삭제
   curl "http://localhost:8080/RANDOM_CODE?delete=DELETE_TOKEN"
   ```
   ```
   deleted
   ```

7. **비밀번호 링크**: `usepassword: true` 헤더를 사용한 비공개 업로드 링크 생성을 지원합니다.

   헤더 사용 시 **영문 대문자 + 영문 소문자 + 숫자 + 특수문자** 조합으로 생성된 8자리 비밀번호가 발급됩니다. 파일은 `?password=...` 쿼리 파라미터 또는 `paste-password: ...` 헤더를 통해 접근할 수 있습니다.
   ```
   # 비밀번호 링크 생성
   curl -H "usepassword: true" -F "file=@secret.txt" http://localhost:8080/

   # 파일 확인: 헤더 방식
   curl -H "paste-password: RANDOM_PASSWORD" http://localhost:8080/RANDOM_CODE

   # 파일 확인: 쿼리 파라미터 방식
   curl "http://localhost:8080/RANDOM_CODE?password=RANDOM_PASSWORD"
   ```

8. **업로드 응답 형식**: 업로드가 성공하면 URL, 만료 시간, 삭제 링크가 반환됩니다. 비밀번호 링크인 경우 `password` 항목도 함께 반환됩니다.
   ```
   url: http://localhost:8080/RANDOM_CODE
   expires: 2026-06-24T05:10:26Z
   password: RANDOM_PASSWORD
   delete: http://localhost:8080/RANDOM_CODE?delete=DELETE_TOKEN
   ```

9. **브라우저에서 내용 복사**: 텍스트 기반 업로드 링크를 브라우저에서 열면 `Raw` 버튼 옆의 `Copy` 버튼으로 내용을 클립보드에 복사할 수 있습니다.

10. **텍스트 파일 브라우저 표시**: `.txt`, `.log` 같은 텍스트 기반 파일은 다운로드되지 않고 브라우저에서 바로 표시됩니다. 원본 raw 응답이 필요하면 `?raw=1`을 사용할 수 있습니다.

11. **생성/삭제 로그**: 파일 생성 및 삭제 시 컨테이너 로그에 기록됩니다.
   ```
   created: id=AbC12 remote=127.0.0.1:51234 size=123 content_type="text/plain; charset=utf-8" policy=temporary expires=2026-06-24T05:10:26Z protected=false
   deleted: id=AbC12 remote=127.0.0.1:51234
   ```

12. **세분화된 락 매니저**: 업로드 ID별로 락을 적용하여 같은 파일에 대한 조회, 삭제, 만료 정리 작업이 동시에 발생해도 충돌을 줄입니다. 서로 다른 파일은 병렬로 처리됩니다.

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
