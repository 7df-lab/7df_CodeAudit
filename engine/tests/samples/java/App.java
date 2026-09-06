package app;

public class App {
    public void query(String id) {
        // BAD: SQL injection
        String q = "SELECT * FROM users WHERE id = " + id;
        System.out.println(q);
    }
}
