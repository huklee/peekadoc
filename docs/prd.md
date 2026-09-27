# 맥미니 원격 개발 서버 구축 및 보안 결과물 확인 환경 보고서

## 1. 개요 및 목적
* **목적:** 외부에 있는 맥북(클라이언트)에서 집에 있는 맥미니(호스트)에 원격 접속하여 CLI 기반 작업(Claude CLI 등) 수행.
* **주요 제약 및 보안 조건:**
    1. **클라이언트 보안 규정(MDM):** 맥북에서 외부로의 파일 유출/반출 엄격 금지.
    2. **파일 섞임 차단:** 실수로라도 맥북의 파일이 맥미니로 역유입되는 상황을 원천 차단 (양방향 파일 공유/SMB 지양).
    3. **클라이언트 흔적 최소화:** 맥북 디스크 내 데이터 잔재가 남지 않는 단방향(맥미니 → 맥북) 읽기 전용(Read-Only) 결과 확인 환경 구축.

---

## 2. 원격 네트워크 및 접속 환경 (Tailscale + SSH)
* **네트워크 구성:** **Tailscale(Personal Free)** mesh VPN 이용
    * 포트포워딩 및 외부 IP 노출 없이 맥미니와 맥북을 가상 사설망으로 안전하게 바인딩.
    * 개인 무료 요금제로 무제한 기기 연결 및 암호화 통신 지원.
* **터미널 접속:** **Tailscale SSH**
    * 명령 대역폭 사용량이 거의 없어 카페/핫스팟 환경에서도 저지연·초고속 접속.
    * 별도 SSH 키 교환 없이 Tailscale 인증 계정으로 보안 접속.
* **필수 백그라운드 세션 관리:** **`tmux`**
    * 맥북 덮개를 닫거나 네트워크 끊김 발생 시에도 맥미니 내 Claude CLI 및 장시간 스크립트 실행 세션 유지.

> ⚠️ **주의사항 (CLI 샌드박스 이슈):**
> Mac App Store 버전 Tailscale은 macOS 샌드박스 제약으로 CLI 커맨드(`tailscale serve`, `tailscale set --ssh` 등) 실행 시 `Fatal error`가 발생할 수 있습니다. 스탠드얼론 버전(`brew install --cask tailscale`) 사용을 권장합니다.

---

## 3. 결과물 확인 환경 구성 (보안/MDM 완벽 준수)

SMB 드라이브 마운트 방식은 양방향 파일 이송 위험으로 배제하며, 아래 3가지 **단방향/읽기 전용** 방식을 조합해 활용합니다.

### A. 마크다운 & 문서 결과물: MkDocs (+ Material 테마)
* **특징:** Notion/GitBook 수준의 최상급 마크다운 웹 문서화 엔진.
* **보안성:** 맥미니 파일 시스템 기반의 완전한 읽기 전용 웹 서버로, 맥북에서의 파일 업로드/유입 가능성 0%.
* **주요 기능:**
    * LaTeX 수식, GFM 표, Mermaid 차트, 코드 구문 강조(Syntax Highlighting) 완벽 지원.
    * 핫 리로딩(Hot Reloading): 맥미니에서 `.md` 파일 수정 시 브라우저 화면 즉시 자동 갱신.
* **실행 예시 (맥미니):**

```bash
pip3 install mkdocs mkdocs-material
cd ~/output && mkdocs serve -a 0.0.0.0:8000
```

### B. 파일 탐색 및 이미지/PDF 확인: Filebrowser (`--read-only`)

* **특징:** 대시보드형 웹 파일 매니저.
* **보안성:** `--read-only` 및 `--noauth` 옵션으로 실행하여 수정/삭제/업로드 버튼이 완전히 제거된 조회 전용 상태 유지.
* **실행 예시 (맥미니):**

```bash
filebrowser --root ~/output --port 8080 --noauth --read-only
```

### C. 클라이언트 데이터 흔적 '0' 확인: VS Code Remote - SSH

* **특징:** 맥북 VS Code를 맥미니 SSH로 연결하여 내장 터미널 및 에디터 활용.
* **장점:**
    * 맥미니에서 열린 포트(예: MkDocs `8000` 포트)를 `localhost:8000`으로 자동 포트 포워딩.
    * 맥북 디스크에 어떠한 파일도 다운로드되지 않으며, 세션 종료 시 잔재가 전혀 남지 않음.

---

## 4. 최종 권장 워크플로우 요약

```
[맥북 (클라이언트)]
   │
   ├─ 1. Terminal (Ghostty / VS Code) ──(Tailscale SSH / tmux)──> [맥미니 (호스트)]
   │                                                                 │
   │                                                         Claude CLI 작업 실행
   │                                                                 │
   ├─ 2. Web Browser (http://localhost:8000) <──(Port Forward)───────┴─ MkDocs (결과물 렌더링)
   │     (수식/차트/리포트 완벽 열람 / Read-Only)
   │
   └─ 3. Web Browser (http://[맥미니-IP]:8080) <──(Tailscale Network)── Filebrowser (--read-only)
         (이미지/PDF/폴더 탐색 / Read-Only)
```

1. **작업 수행:** 맥북에서 SSH로 접속 후 `tmux` 세션 내에서 Claude CLI에 작업지시.
2. **문서/리포트 검토:** 자동 포트 포워딩된 `localhost:8000` 브라우저 접속을 통해 MkDocs로 고품질 마크다운 결과물 확인.
3. **일반 파일 검토:** Filebrowser 읽기 전용 웹 UI 접속으로 이미지 및 기타 생성 파일 확인.
