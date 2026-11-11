// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"encoding/json"
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	lens "github.com/ElcanoTek/lens"
	"github.com/ElcanoTek/lens/internal/i18n"
)

// This checks authored messages in both renderers against both catalogs. Names
// and machine IDs arrive as data and deliberately are not catalog keys.
func TestAuthoredBrowserMessagesHaveTranslations(t *testing.T) {
	call := regexp.MustCompile(`(?:\b[TH]\(|\{\{tr\s+)("(?:\\.|[^"\\])*")`)
	for _, language := range []string{"es", "pt"} {
		var catalog map[string]string
		if err := json.Unmarshal([]byte(i18n.CatalogJSON(language)), &catalog); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"static/main.js", "static/custom-categories.js", "templates/index.html", "templates/signed_out.html", "templates/locale.html"} {
			source := lens.Static
			if path[:9] == "templates" {
				source = lens.Templates
			}
			raw, err := fs.ReadFile(source, path)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range call.FindAllStringSubmatch(string(raw), -1) {
				key, err := strconv.Unquote(match[1])
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := catalog[key]; !ok {
					t.Errorf("%s: %s missing %q", path, language, key)
				}
			}
		}
	}
}

func TestBrowserLanguagePreferenceAndEscaping(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is an optional developer tool for browser-runtime tests")
	}
	raw, err := fs.ReadFile(lens.Static, "static/language.js")
	if err != nil {
		t.Fatal(err)
	}
	// Execute the shipped plain script in an isolated DOM facade. Browser visual
	// and interaction checks also exercise it with Chromium during UI review.
	script := `const vm=require('node:vm'); const assert=require('node:assert/strict');
 const source=JSON.parse(process.argv[1]);
 function boot(lang,url,saved,blocked=false){
  const state={replaced:null,stored:saved,assigned:null};
  const root={lang,dataset:{theme:'light'}}; const events={};
  const catalog={'Deleted {0}':'Eliminado {0}'};
  const context={URL,Intl,window:{location:{href:url,replace:u=>state.replaced=u,assign:u=>state.assigned=u}},
   localStorage:{getItem:()=>{if(blocked)throw Error('disabled');return state.stored},setItem:(k,v)=>{if(blocked)throw Error('disabled');assert.equal(k,'elcano.language');state.stored=v}},
   document:{documentElement:root,querySelector:s=>s.startsWith('meta')?{content:JSON.stringify(catalog)}:null,querySelectorAll:()=>[],addEventListener:(e,fn)=>events[e]=fn}};
  vm.runInNewContext(source,context);return {state,runtime:context.window.LensI18n};
 }
 let a=boot('es','https://lens.example/?lang=es-MX','pt');assert.equal(a.state.stored,'es');assert.equal(a.state.replaced,null);
 assert.equal(a.runtime.text('Deleted {0}','{1}<customer>'),'Eliminado {1}<customer>');assert.equal(a.runtime.html('Deleted {0}','<img onerror="bad">'),'Eliminado &lt;img onerror=&quot;bad&quot;&gt;');
 assert.equal(a.runtime.number(12345),'12.345');
 a=boot('en','https://lens.example/?filter=customer#files','pt-BR');assert.equal(new URL(a.state.replaced).searchParams.get('lang'),'pt');assert.equal(new URL(a.state.replaced).searchParams.get('filter'),'customer');assert.equal(new URL(a.state.replaced).hash,'#files');
 a=boot('pt','https://lens.example/?lang=pt',null,true);assert.equal(a.runtime.language,'pt');
 a=boot('en','https://lens.example/?lang=unsupported','invalid');assert.equal(a.state.replaced,null);
 `
	encoded, _ := json.Marshal(string(raw))
	output, err := exec.Command(node, "-e", script, string(encoded)).CombinedOutput()
	if err != nil {
		t.Fatalf("language runtime: %v\n%s", err, output)
	}
}
