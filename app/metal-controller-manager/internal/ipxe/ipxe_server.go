// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package ipxe

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"text/template"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/talos-systems/sidero/app/metal-controller-manager/api/v1alpha1"
	"github.com/talos-systems/sidero/app/metal-controller-manager/internal/server"
	agentclient "github.com/talos-systems/sidero/app/metal-controller-manager/pkg/client"
)

const bootFile = `#!ipxe
chain ipxe?uuid=${uuid}&mac=${mac:hexhyp}&domain=${domain}&hostname=${hostname}&serial=${serial}
`

var ipxeTemplate = template.Must(template.New("iPXE config").Parse(`#!ipxe
kernel /env/{{ .Env.Name }}/vmlinuz {{range $arg := .Env.Spec.Kernel.Args}} {{$arg}}{{end}}
initrd /env/{{ .Env.Name }}/initramfs.xz
boot
`))

var apiEndpoint string

func bootFileHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, bootFile)
}

func ipxeHandler(w http.ResponseWriter, r *http.Request) {
	var (
		config *rest.Config
		err    error
	)

	kubeconfig, ok := os.LookupEnv("KUBECONFIG")
	if ok {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Printf("error creating config: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Printf("error creating config: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}

	c, err := agentclient.NewClient(config)
	if err != nil {
		log.Printf("error creating client: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
	}

	labels := labelsFromRequest(r)

	log.Printf("UUID: %q", labels["uuid"])

	key := client.ObjectKey{
		Name: labels["uuid"],
	}

	obj := &metalv1alpha1.Server{}

	if err := c.Get(context.Background(), key, obj); err != nil {
		// If we can't find the server then we know that discovery has not been
		// performed yet.
		if apierrors.IsNotFound(err) {
			args := struct {
				Env metalv1alpha1.Environment
			}{
				Env: metalv1alpha1.Environment{
					ObjectMeta: v1.ObjectMeta{
						Name: "discovery",
					},
					Spec: metalv1alpha1.EnvironmentSpec{
						Kernel: metalv1alpha1.Kernel{
							Args: []string{
								"initrd=initramfs.xz",
								"page_poison=1",
								"slab_nomerge",
								"slub_debug=P",
								"pti=on",
								"panic=0",
								"random.trust_cpu=on",
								"ima_template=ima-ng",
								"ima_appraise=fix",
								"ima_hash=sha512",
								"ip=dhcp",
								"console=tty0",
								"console=ttyS0",
								"sidero.endpoint=" + fmt.Sprintf("%s:%s", apiEndpoint, server.Port),
							},
						},
					},
				},
			}

			var buf bytes.Buffer

			err = ipxeTemplate.Execute(&buf, args)
			if err != nil {
				log.Printf("error rendering template: %v", err)
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			if _, err := buf.WriteTo(w); err != nil {
				log.Printf("error writing to response: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
			}

			return
		}

		log.Printf("error looking up server: %v", err)
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	var env metalv1alpha1.Environment

	if err := determineEnvironment(c, obj, &env); err != nil {
		if apierrors.IsNotFound(err) {
			log.Printf("environment not found: %v", err)
			w.WriteHeader(http.StatusNotFound)

			return
		}
	}

	args := struct {
		Env metalv1alpha1.Environment
	}{
		Env: env,
	}

	var buf bytes.Buffer

	err = ipxeTemplate.Execute(&buf, args)
	if err != nil {
		log.Printf("error rendering template: %v", err)
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("error writing to response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func ServeIPXE(endpoint string) error {
	apiEndpoint = endpoint

	mux := http.NewServeMux()

	mux.Handle("/boot.ipxe", logRequest(http.HandlerFunc(bootFileHandler)))
	mux.Handle("/ipxe", logRequest(http.HandlerFunc(ipxeHandler)))
	mux.Handle("/env/", logRequest(http.StripPrefix("/env/", http.FileServer(http.Dir("/var/lib/sidero/env")))))

	log.Println("Listening...")

	return http.ListenAndServe(":8081", mux)
}

func logRequest(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("HTTP %s %v %s", r.Method, r.URL, r.RemoteAddr)
		next.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
}

func labelsFromRequest(req *http.Request) map[string]string {
	values := req.URL.Query()

	labels := map[string]string{}

	for key := range values {
		switch strings.ToLower(key) {
		case "mac":
			// set mac if and only if it parses
			if hw, err := parseMAC(values.Get(key)); err == nil {
				labels[key] = hw.String()
			}
		default:
			// matchers don't use multi-value keys, drop later values
			labels[key] = values.Get(key)
		}
	}

	return labels
}

func parseMAC(s string) (net.HardwareAddr, error) {
	macAddr, err := net.ParseMAC(s)
	if err != nil {
		return nil, err
	}

	return macAddr, err
}

// determineEnvionment handles which env CRD we'll respect for a given server.
// specied in the server spec overrides everything, specified in the server class overrides default, default is default :).
func determineEnvironment(c client.Client, serverObj *metalv1alpha1.Server, envObj *metalv1alpha1.Environment) error {
	envName := "default"

	if serverObj.Spec.EnvironmentRef != nil {
		envName = serverObj.Spec.EnvironmentRef.Name
	} else if serverObj.OwnerReferences != nil {
		// search for serverclass in owner refs. if found, fetch it and see if it's got an env ref
		for _, owner := range serverObj.OwnerReferences {
			if owner.Kind == "ServerClass" {
				serverClassResource := &metalv1alpha1.ServerClass{}

				if err := c.Get(context.Background(), types.NamespacedName{Namespace: "", Name: owner.Name}, serverClassResource); err != nil {
					return err
				}

				if serverClassResource.Spec.EnvironmentRef != nil {
					envName = serverClassResource.Spec.EnvironmentRef.Name
				}
			}
		}
	}

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "", Name: envName}, envObj); err != nil {
		return err
	}

	return nil
}
