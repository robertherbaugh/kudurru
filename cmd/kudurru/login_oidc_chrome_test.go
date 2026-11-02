package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Qualification is opt-in because ordinary unit jobs need no installed browser.
// The OP URL travels on stdin, never process arguments or diagnostic output.
func TestCLIOIDCBrowserInstalledChromeActualTLS(t *testing.T) {
	module := os.Getenv("KUDURRU_TEST_PLAYWRIGHT_MODULE")
	chrome := os.Getenv("KUDURRU_TEST_CHROME")
	if module == "" || chrome == "" {
		t.Skip("installed-browser qualification requires explicit executable/module")
	}
	f := cliPublicFixture(t)
	der, e := x509.MarshalPKIXPublicKey(f.op.Server.Certificate().PublicKey)
	if e != nil {
		t.Fatal("OP public key")
	}
	hash := sha256.Sum256(der)
	open := func(ctx context.Context, target string) error {
		script := `const fs=require('node:fs');(async()=>{const p=JSON.parse(fs.readFileSync(0,'utf8'));const {chromium}=require(p.module);const b=await chromium.launch({executablePath:p.chrome,args:['--no-sandbox','--ignore-certificate-errors-spki-list='+p.spki]});try{const page=await b.newPage();await page.goto(p.target);if(!(await page.textContent('body')).includes('Login response received.'))throw Error('callback');}finally{await b.close();}})().catch(e=>{const s=String(e);process.stderr.write(s.includes('ERR_SSL_VERSION_OR_CIPHER_MISMATCH')?'tls-cipher':s.includes('ERR_CERT_')?'tls-certificate':s.includes('Timeout')?'timeout':'browser-other');process.exit(1);});`
		input, _ := json.Marshal(map[string]string{"module": module, "chrome": chrome, "spki": base64.StdEncoding.EncodeToString(hash[:]), "target": target})
		defer clear(input)
		cmd := exec.CommandContext(ctx, "node", "-e", script)
		cmd.Stdin = strings.NewReader(string(input))
		cmd.Stdout = io.Discard
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		if cmd.Run() != nil {
			switch diagnostic.String() {
			case "tls-cipher", "tls-certificate", "timeout", "browser-other":
				t.Log("sanitized browser stage: " + diagnostic.String())
			default:
				t.Log("sanitized browser stage: process")
			}
			return errors.New("installed browser qualification failed")
		}
		return nil
	}
	_, _, _, e = runActualCLILogin(t, f, loginOIDCOptions{open: open}, nil, false)
	if e != nil {
		t.Fatalf("installed Chrome public login refused at %s", cliLoginErrorPhase(e))
	}
	if f.op.requests.Load() != 1 || len(f.security(t).ExternalSessions) != 0 {
		t.Fatal("browser created extra credential authority")
	}
}
