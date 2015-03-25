package main

import (
	"encoding/hex"
	"net/http"
	"text/template"
)

func (r *redirector) powershell(w http.ResponseWriter, req *http.Request) {
	const name = "/flynn-windows-386.gz"
	f, ok := r.targets()[name]
	if !ok {
		http.Error(w, "unknown target", 404)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	data := struct{ URL, Checksum string }{r.url(name, f), hex.EncodeToString(f.Hashes["sha512"])}
	psInstall.Execute(w, data)
}

var psInstall = template.Must(template.New("install.ps1").Parse(`
# Flynn CLI PowerShell installer
# Copyright (c) 2015 Prime Directive, Inc. All rights reserved.
#
# LICENSE: https://github.com/flynn/flynn/blob/master/LICENSE

$url = "{{.URL}}"
$checksum = "{{.Checksum}}"
$destDir = "$Env:APPDATA\flynn\bin"
$flynn = "$destDir\flynn.exe"

# download the gzipped exe
$gzipped = (New-Object Net.WebClient).DownloadData($url)

# verify the checksum
$sha512 = [Security.Cryptography.HashAlgorithm]::Create("SHA512")
$actualChecksum = -Join ($sha512.ComputeHash($gzipped) | ForEach { "{0:x2}" -f $_ })
If ($actualChecksum -ne $checksum) {
  Throw "Expected checksum to be $checksum but got $actualChecksum"
}

# create the destination directory
New-Item -Path $destDir -ItemType directory -Force | Out-Null

# gunzip exe into destination
$dest = New-Object System.IO.FileStream $flynn,
                                        ([IO.FileMode]::Create),
                                        ([IO.FileAccess]::Write),
                                        ([IO.FileShare]::None)
$exeStream = New-Object System.IO.Compression.GzipStream (New-Object System.IO.MemoryStream(,$gzipped)),
                                                         ([IO.Compression.CompressionMode]::Decompress)
$buf = New-Object byte[](1024)
While ($true) {
  $n = $exeStream.Read($buf, 0, 1024)
  If ($n -le 0) { Break }
  $dest.Write($buf, 0, $n)
}
$dest.Close()

# ensure added to path in registry
$regPath = [Environment]::GetEnvironmentVariable("PATH", "User")
If ($regPath -notcontains $destDir) {
  [Environment]::SetEnvironmentVariable("PATH", $regPath + ";" + $destDir, "User")
}

# ensure added to path for current session
If ($Env:Path -notcontains $destDir) {
  $Env:Path += ";" + $destDir
}

Write-Host "Flynn CLI installed. Run 'flynn help' to try it out."
`[1:]))
