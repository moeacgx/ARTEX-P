import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("mention trigger supports Chinese and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("已选 @[漏洞#1 X]", 12), null);
  assert.deepEqual(activeMention("查看@漏洞 后面的文字", 5), { start: 2, end: 5, query: "漏洞" });
  assert.equal(activeMention("@漏洞\n下一行", 8), null);
});

test("categories, Chinese aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 10);
  assert.equal(mentionSearch("漏").categories[0].kind, "finding");
  assert.equal(mentionSearch("漏洞").kind, "finding");
  assert.equal(mentionSearch("漏洞SQL注入").query, "SQL注入");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("接口 GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("技能类型可从菜单、中文别名和英文别名搜索", () => {
  assert.equal(mentionSearch("").categories[0].kind, "skill");
  assert.equal(mentionSearch("技").categories[0].kind, "skill");
  assert.equal(mentionSearch("skill fofa").kind, "skill");
  assert.equal(mentionSearch("skill fofa").query, "fofa");
  assert.equal(mentionSearch("技能资产发现").query, "资产发现");
});

test("技能名称作为引用标识且不会重复附加标签", () => {
  for (const name of ["fofa-search", "中文技能", "技能-二", "日本語スキル"]) {
    const token = mentionToken({ kind: "skill", id: name, label: name, description: "描述" });
    assert.equal(token, `@[技能#${name}]`);
    assert.equal(activeMention(token, token.length), null);
    assert.deepEqual(selectedMentions(`使用 ${token}`), [{ token, label: `技能 · ${name}`, start: 3 }]);
  }
});

test("移除中文技能引用后保留其他技能与既有数字记录", () => {
  const value = "使用 @[技能#中文技能]、@[漏洞#12 标题] 和 @[技能#fofa-search]";
  const selected = selectedMentions(value);
  assert.equal(selected.length, 3);
  assert.equal(selected[1].label, "漏洞 #12 · 标题");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.deepEqual(
    selectedMentions(next).map((item) => item.token),
    ["@[漏洞#12 标题]", "@[技能#fofa-search]"],
  );
});

test("未闭合或跨行的技能引用不生成徽标", () => {
  assert.deepEqual(selectedMentions("@[技能#] @[技能#foo\nbar] @[技能#fofa-search"), []);
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "标题[1]\n描述" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `分析 ${first} 和 ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "漏洞 #12 · 标题（1） 描述");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
