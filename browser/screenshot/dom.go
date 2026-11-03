package screenshot

// This function only reads the main document and returns bounded text. It never
// writes DOM, follows frames, reads form values or returns raw markup/URLs.
const snapshotDOM = `function(limits) {
function visible(el) {
  for(let depth=0;el&&el.nodeType===1;el=el.parentElement) {
    if(++depth>64||el.hidden||el.getAttribute('aria-hidden')==='true') return false;
    const style=getComputedStyle(el);
    if(style.display==='none'||style.visibility==='hidden'||style.visibility==='collapse'||style.opacity==='0') return false;
  }
  return true;
}
const stack=[document.body||document.documentElement],parts=[];
let size=0,visited=0,truncated=false;
while(stack.length&&visited++<5000&&size<limits.text) {
  const node=stack.pop();if(!node) continue;
  if(node.nodeType===3) {
    const raw=node.nodeValue||'', text=raw.slice(0,limits.text-size);
    if(raw.length>text.length) truncated=true;
    if(text.trim()) {parts.push(text);size+=text.length+1;}
  } else if(node.nodeType===1&&visible(node)&&!/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE|INPUT|TEXTAREA|SELECT|OPTION|IFRAME|FRAME)$/.test(node.tagName)&&!node.isContentEditable) {
    const children=node.childNodes;
    if(children.length>200) truncated=true;
    for(let i=Math.min(children.length,200)-1;i>=0;i--) {
      if(stack.length>=6000) {truncated=true;break;}
      stack.push(children[i]);
    }
  }
}
return {title:document.title.slice(0,limits.title),text:parts.join(' ').slice(0,limits.text),truncated:truncated||stack.length>0||document.title.length>limits.title};
}`
