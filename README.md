# Windows Compile Service

## Project Structure

```text
.
├── cmd
│   └── server
│       └── main.go
├── internal
│   ├── api
│   │   └── handler.go
│   ├── compiler
│   │   └── msvc.go
│   ├── router
│   │   └── router.go
│   └── utils
│       └── zip.go
├── go.mod
└── go.sum
```

## Build

```powershell
go mod tidy
GOOS=windows GOARCH=amd64 go build -o msvc-compile-service.exe ./cmd/server
```

## Run

```powershell
$env:HTTP_ADDR=":8080"
.\bin\compile-service.exe
```

## cURL

```bash
curl -X POST "http://windows-server:8080/compile" \
  -H "Content-Type: application/json" \
  --data-binary '{
    "source_code": "#include <stdio.h>\nint main(void){printf(\"hello\\n\");return 0;}",
    "compiler_options": {
      "opt_level": "O2",
      "no_inline": true,
      "keep_frame_pointer": true
    }
  }' \
  -D headers.txt \
  --output compile-response.multipart
```

## Go Example

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
)

type CompileRequest struct {
	SourceCode      string                  `json:"source_code"`
	CompilerOptions CompileRequestOptions   `json:"compiler_options"`
}

type CompileRequestOptions struct {
	OptLevel         string `json:"opt_level"`
	NoInline         bool   `json:"no_inline"`
	KeepFramePointer bool   `json:"keep_frame_pointer"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func main() {
	reqBody := CompileRequest{
		SourceCode: "#include <stdio.h>\nint main(void){printf(\"hello\\n\");return 0;}",
		CompilerOptions: CompileRequestOptions{
			OptLevel:         "O2",
			NoInline:         true,
			KeepFramePointer: true,
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		panic(err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://windows-server:8080/compile", bytes.NewReader(payload))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var er ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
			panic(fmt.Errorf("request failed with status %d", resp.StatusCode))
		}
		panic(fmt.Errorf("request failed with status %d: %s", resp.StatusCode, er.Error))
	}

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		panic(err)
	}
	if mediaType != "multipart/form-data" {
		panic(fmt.Errorf("unexpected content type: %s", mediaType))
	}

	reader := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}

		data, err := io.ReadAll(part)
		if err != nil {
			panic(err)
		}

		switch part.FormName() {
		case "artifact":
			if err := os.WriteFile("sample.exe", data, 0o644); err != nil {
				panic(err)
			}
			fmt.Printf("saved sample.exe (%d bytes)\n", len(data))
		case "build_log":
			fmt.Printf("build_log:\n%s\n", string(data))
		case "meta":
			fmt.Printf("meta: %s\n", string(data))
		}
	}

	fmt.Printf("X-Compile-Return-Code: %s\n", resp.Header.Get("X-Compile-Return-Code"))
	fmt.Printf("X-Compile-Timed-Out: %s\n", resp.Header.Get("X-Compile-Timed-Out"))
}
```

## Windows Firewall

```powershell
New-NetFirewallRule `
  -DisplayName "Compile Service 8080" `
  -Direction Inbound `
  -Action Allow `
  -Protocol TCP `
  -LocalPort 8080
```

## Install Visual Studio Build Tools

1. Download Visual Studio Build Tools installer:
   `https://visualstudio.microsoft.com/downloads/`
2. Run the installer.
3. Select `C++ build tools`.
4. Ensure these components are installed:
   - `MSVC v143 - VS 2022 C++ x64/x86 build tools`
   - `Windows 10 SDK` or `Windows 11 SDK`
   - `VC++ 2022 version of the C++ x64/x86 build tools`
5. Confirm `vswhere.exe` exists at:
   `C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe`

## Run as a Windows Service

### Option 1: Native `sc.exe`

```powershell
sc.exe create MSVCCompileService binPath= "C:\BIN\msvc-compile-service.exe" start= auto
sc.exe start MSVCCompileService
sc.exe query MSVCCompileService
```

### Option 2: NSSM

```powershell
nssm install CompileService C:\services\compile-service\compile-service.exe
nssm set CompileService AppDirectory C:\services\compile-service
nssm set CompileService AppEnvironmentExtra HTTP_ADDR=:8080
nssm start CompileService
```

## Deploy

1. Copy the built `compile-service.exe` to the Windows Server.
2. Install Visual Studio Build Tools with the C++ toolchain.
3. Create the work root:
   ```powershell
   New-Item -ItemType Directory -Force C:\build\tmp
   ```
4. Compile artifacts are retained per request under `C:\build\tmp\<request-id>`:
   - `sample.c`
   - `build.cmd`
   - `build.log`
   - `meta.json`
   - `sample.exe` (if compilation succeeds)
5. Open the firewall port.
6. Start the process directly or register it as a Windows service.
