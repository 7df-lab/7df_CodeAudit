var express = require('express');
var app = express();

app.get('/eval', function(req, res) {
  // BAD: eval with user input
  var result = eval(req.query.code);
  res.send(String(result));
});

app.listen(3000);
