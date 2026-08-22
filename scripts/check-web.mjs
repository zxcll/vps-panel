// 前端组件的编译期自检。
//
// 存在的理由：Vue 的模板是运行时编译的，模板里但凡有个表达式写错，
// 报错只会出现在浏览器控制台里，而页面**整片白屏** —— Go 那边的测试
// 一个都测不到，e2e 也只查得到 HTTP 200。
//
// 踩过的坑长这样：
//
//     @click="copy(list.join('\n'))"
//
// 模板是用反引号写的，JS 会先把 \n 转成真正的换行符，Vue 拿到的
// 就成了一个跨行的字符串字面量，编译直接抛 SyntaxError，
// 整个「转发规则」页面白屏。写成 \\n 才对。
//
// 这个脚本把每个组件的 template 拿去真的编译一遍，有问题当场报出来。
//
// 它是**可选**的：项目本身不需要 Node（前端是浏览器直接可运行的 ES 模块）。
// 机器上没有 Node 时 make check-web 会跳过，不影响构建和发布。

import { createRequire } from "node:module";
import { mkdtempSync, readFileSync, writeFileSync, readdirSync, rmSync, cpSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";
import { pathToFileURL } from "node:url";

const ASSETS = new URL("../web/assets/", import.meta.url);
const VENDOR = new URL("../web/vendor/vue.esm-browser.prod.js", import.meta.url);

// 浏览器里 "vue" 这个裸导入是靠 index.html 的 importmap 解析的。
// Node 没有 importmap，所以把源码复制一份出来，把裸导入改写成相对路径。
function stageModules() {
    const dir = mkdtempSync(join(tmpdir(), "vps-panel-web-"));
    cpSync(new URL(VENDOR), join(dir, "vue.js"));

    const files = readdirSync(new URL(ASSETS)).filter((f) => f.endsWith(".js"));
    for (const f of files) {
        const src = readFileSync(new URL(f, ASSETS), "utf8");
        writeFileSync(join(dir, f), src.replace(/from\s+["']vue["']/g, 'from "./vue.js"'));
    }
    return { dir, files };
}

// decodeEntities 把常见的 HTML 实体还原。
//
// 认不出来的 & 原样留着 —— 浏览器就是这么处理的，而模板里的 & 绝大多数
// 是表达式里的 &&，本来就不是实体。
const NAMED = {
    "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": '"',
    "&apos;": "'", "&nbsp;": " ",
};

function decodeEntities(s) {
    return s.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (m) => {
        if (NAMED[m]) return NAMED[m];
        if (m[1] === "#") {
            const code = m[2] === "x" || m[2] === "X"
                ? parseInt(m.slice(3, -1), 16)
                : parseInt(m.slice(2, -1), 10);
            return Number.isFinite(code) ? String.fromCodePoint(code) : m;
        }
        return m;
    });
}

// 浏览器 API 的最小替身。组件在 import 阶段会碰到 localStorage 之类的东西
// （api.js 一加载就读 token），缺了它模块根本导入不了。
function stubBrowserGlobals() {
    const store = new Map();
    globalThis.localStorage = {
        getItem: (k) => (store.has(k) ? store.get(k) : null),
        setItem: (k, v) => store.set(k, String(v)),
        removeItem: (k) => store.delete(k),
    };
    globalThis.window = globalThis;
    globalThis.location = { hash: "#/overview" };

    // Vue 的 esm-browser 构建在加载时就会摸 document（建模板容器之类），
    // 少一个方法就会在 import 阶段直接抛，而且报错是一大坨压缩源码。
    //
    // 注意这个替身**不需要**实现 innerHTML/textContent 那套：浏览器版 Vue
    // 解码 HTML 实体用的是「往 div 里塞 innerHTML 再读回来」的土办法，
    // 但 compile() 允许传自己的 decodeEntities，我们走那条路，完全不碰 DOM。
    const el = () => ({
        setAttribute() {},
        appendChild() {},
        addEventListener() {},
        removeEventListener() {},
        style: {},
    });
    globalThis.document = {
        documentElement: el(),
        head: el(),
        body: el(),
        createElement: el,
        createElementNS: el,
        createTextNode: el,
        createComment: el,
        querySelector: () => null,
        addEventListener() {},
        removeEventListener() {},
    };
    globalThis.addEventListener = () => {};
    globalThis.removeEventListener = () => {};
    // navigator 在新版 Node 里是只读的内置对象，赋值会抛。
    // 组件只用到 navigator.clipboard，而且是可选链，没有也不影响导入。
    globalThis.fetch = async () => {
        throw new Error("检查脚本不该发起真实请求");
    };
}

// walk 递归找出一个模块导出里所有带 template 的组件，包括嵌套的子组件。
function collectComponents(value, path, out, seen = new Set()) {
    if (!value || typeof value !== "object" || seen.has(value)) return;
    seen.add(value);

    if (typeof value.template === "string") {
        out.push({ path, template: value.template });
    }
    // components: { Foo, Bar } 里的子组件同样要检查 —— 它们是单独定义的对象，
    // 常常不从模块导出，只在父组件里引用。
    for (const [k, v] of Object.entries(value)) {
        if (k === "template" || k === "setup") continue;
        collectComponents(v, `${path}.${k}`, out, seen);
    }
}

async function main() {
    stubBrowserGlobals();
    const { dir, files } = stageModules();

    let checked = 0;
    const failures = [];

    try {
        const { compile } = await import(pathToFileURL(join(dir, "vue.js")).href);

        for (const file of files) {
            // vendor 的 vue 自己不用查。
            if (file === "vue.js") continue;

            let mod;
            try {
                mod = await import(pathToFileURL(join(dir, file)).href);
            } catch (err) {
                failures.push({ where: file, msg: `模块加载失败：${err.message}` });
                continue;
            }

            const found = [];
            for (const [name, exported] of Object.entries(mod)) {
                collectComponents(exported, `${basename(file)} → ${name}`, found);
            }

            for (const c of found) {
                checked++;
                try {
                    // 真的编译一遍。表达式写错了这里就会抛。
                    // 传自己的 decodeEntities，让编译器不去碰 DOM。
                    compile(c.template, { decodeEntities });
                } catch (err) {
                    failures.push({ where: c.path, msg: err.message });
                }
            }
        }
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }

    if (failures.length) {
        console.error(`\n✗ ${failures.length} 个组件的模板编译不过：\n`);
        for (const f of failures) {
            console.error(`  ${f.where}`);
            console.error(`    ${f.msg}\n`);
        }
        console.error("模板编译失败会让整个页面白屏，浏览器控制台之外没有任何提示。");
        console.error("最常见的原因：模板是反引号字符串，里面的 \\n 要写成 \\\\n。\n");
        process.exit(1);
    }

    if (checked === 0) {
        console.error("✗ 一个组件都没检查到，脚本大概坏了");
        process.exit(1);
    }
    console.log(`✓ ${checked} 个组件的模板全部编译通过`);
}

// createRequire 只是为了让这个文件在被当成 CJS 误用时也能给出像样的报错。
void createRequire;

await main();
