// sdk-client — e2e-проверка фазы 4: штатный go-sdk с InstanceServiceAccount (без env и сайдкаров) в цикле читает
// каталог. Один SDK на весь процесс — заодно проверяется фоновое обновление токена SDK (~раз в 50 с).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	rmapi "github.com/yandex-cloud/go-genproto/yandex/cloud/resourcemanager/v1"
	rmsdk "github.com/yandex-cloud/go-sdk/services/resourcemanager/v1"
	ycsdk "github.com/yandex-cloud/go-sdk/v2"
	"github.com/yandex-cloud/go-sdk/v2/credentials"
	"github.com/yandex-cloud/go-sdk/v2/pkg/options"
)

func main() {
	folder := flag.String("folder", "", "id каталога для Folder.Get")
	interval := flag.Duration("interval", 30*time.Second, "пауза между вызовами")
	flag.Parse()
	if *folder == "" {
		log.Fatal("-folder is required")
	}
	ctx := context.Background()
	sdk, err := ycsdk.Build(ctx, options.WithCredentials(credentials.InstanceServiceAccount()))
	if err != nil {
		log.Fatalf("build sdk: %v", err)
	}
	folders := rmsdk.NewFolderClient(sdk)
	for {
		f, err := folders.Get(ctx, &rmapi.GetFolderRequest{FolderId: *folder})
		if err != nil {
			fmt.Println(time.Now().UTC().Format(time.RFC3339), "err", err)
		} else {
			fmt.Println(time.Now().UTC().Format(time.RFC3339), "ok", f.GetName())
		}
		time.Sleep(*interval)
	}
}
